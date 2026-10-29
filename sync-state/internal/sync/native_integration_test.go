package sync

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/buffers"
	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/readmodel"
	"sync-state/internal/rpc"
	"sync-state/internal/staking"
)

const nativeWorkerAddress = "structs1q3nx9j4qcqaf4p2qjc9zxqxrccg4renc2fyaw5"
const nativeWorkerOther = "structs1rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"

func nativeWorkerFixture(t *testing.T) (*Syncer, context.Context) {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	chain := "native-worker-" + t.Name()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sync_state.native_staking_snapshot WHERE chain_id=$1`, chain)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sync_state.native_staking_invalidation WHERE chain_id=$1`, chain)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sync_state.sync_cursor WHERE chain_id=$1`, chain)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sync_state.sync_cursor(chain_id,last_height,last_block_time,status)
VALUES ($1,100,'2026-10-29T12:00:00Z','current')`, chain); err != nil {
		t.Fatal(err)
	}
	s := NewSyncer(Config{NativeStakingRefresh: true}, nil, &db.Pool{Pool: pool}, nil, chain)
	s.logger = log.New(io.Discard, "", 0)
	return s, ctx
}

func nativeRequest(t *testing.T, s *Syncer, ctx context.Context, height int64, addresses ...string) {
	t.Helper()
	tx, err := s.pool.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := staking.Request(ctx, tx, s.chainID, height, staking.Changes{Addresses: addresses}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=$2 WHERE chain_id=$1`, s.chainID, height); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func emptyNativeResponse(w http.ResponseWriter, r *http.Request, chain string) {
	if strings.HasSuffix(r.URL.Path, "/node_info") {
		_ = json.NewEncoder(w).Encode(map[string]any{"default_node_info": map[string]string{"network": chain}})
		return
	}
	w.Header().Set("x-cosmos-block-height", r.Header.Get("x-cosmos-block-height"))
	_ = json.NewEncoder(w).Encode(map[string]any{"delegation_responses": []any{}, "unbonding_responses": []any{},
		"redelegation_responses": []any{}, "pagination": map[string]string{"next_key": ""}})
}

func TestNativeWorker_HTTPDoesNotBlockWriterAndOldResultIsDiscarded(t *testing.T) {
	s, ctx := nativeWorkerFixture(t)
	nativeRequest(t, s, ctx, 100, nativeWorkerAddress)
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/delegations/") && blocked.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		emptyNativeResponse(w, r, s.chainID)
	}))
	t.Cleanup(server.Close)
	s.cfg.LCDURL = server.URL
	type result struct {
		count int
		err   error
	}
	done := make(chan result, 1)
	go func() { n, err := s.refreshNativeBatch(ctx); done <- result{n, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("worker never fetched")
	}
	// This commits while the LCD request is still blocked. If the worker
	// holds the maintenance lock across HTTP, this short context catches it.
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	tx, err := s.pool.Pool.Begin(writeCtx)
	if err == nil {
		err = readmodel.Lock(writeCtx, tx)
	}
	if err == nil {
		_, err = staking.Request(writeCtx, tx, s.chainID, 101, staking.Changes{Addresses: []string{nativeWorkerAddress}})
	}
	if err == nil {
		_, err = tx.Exec(writeCtx, `UPDATE sync_state.sync_cursor SET last_height=101 WHERE chain_id=$1`, s.chainID)
	}
	if err == nil {
		err = tx.Commit(writeCtx)
	} else if tx != nil {
		_ = tx.Rollback(context.Background())
	}
	cancel()
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.count != 0 {
		t.Fatalf("stale fetch published: %+v", r)
	}
	n, err := s.refreshNativeBatch(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retry did not publish: %d %v", n, err)
	}
	var complete bool
	var height int64
	var bonded, pending string
	if err := s.pool.Pool.QueryRow(ctx, `SELECT complete,source_height,bonded::text,unbonding::text
FROM sync_state.native_staking_claim WHERE chain_id=$1 AND address=$2`, s.chainID, nativeWorkerAddress).
		Scan(&complete, &height, &bonded, &pending); err != nil || !complete || height != 101 || bonded != "0" || pending != "0" {
		t.Fatalf("wrong published evidence: %v %d %s %s %v", complete, height, bonded, pending, err)
	}
}

func TestNativeWorker_PartialFailureDoesNotStarveOtherAddresses(t *testing.T) {
	s, ctx := nativeWorkerFixture(t)
	nativeRequest(t, s, ctx, 100, nativeWorkerAddress, nativeWorkerOther)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, nativeWorkerAddress+"/unbonding_delegations") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		emptyNativeResponse(w, r, s.chainID)
	}))
	t.Cleanup(server.Close)
	s.cfg.LCDURL = server.URL
	if n, err := s.refreshNativeBatch(ctx); err != nil || n != 1 {
		t.Fatalf("unrelated address was starved: %d %v", n, err)
	}
	var pending, delayed bool
	var source *int64
	if err := s.pool.Pool.QueryRow(ctx, `SELECT pending,retry_after>NOW(),source_height
FROM sync_state.native_staking_snapshot WHERE chain_id=$1 AND address=$2`, s.chainID, nativeWorkerAddress).
		Scan(&pending, &delayed, &source); err != nil || !pending || !delayed || source != nil {
		t.Fatalf("partial snapshot accepted: %v %v %v %v", pending, delayed, source, err)
	}
	if n, err := s.refreshNativeBatch(ctx); err != nil || n != 0 {
		t.Fatalf("failed address ignored its backoff: %d %v", n, err)
	}
}

func TestNativeInvalidation_RollsBackWithCanonicalTransaction(t *testing.T) {
	s, ctx := nativeWorkerFixture(t)
	tx, err := s.pool.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	dirty := readmodel.NewDirty()
	dirty.NativeAddress(nativeWorkerAddress)
	if err := staking.Invalidate(ctx, tx, s.chainID, 100, dirty, staking.Changes{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.pool.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM sync_state.native_staking_snapshot WHERE chain_id=$1`, s.chainID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rollback left native requests: %d %v", rows, err)
	}
}

func TestNativeInvalidation_ActualBlockMarksEmptyCompletionBeforeDeferredProjection(t *testing.T) {
	s, ctx := nativeWorkerFixture(t)
	s.router = events.NewRouter(false)
	nativeRequest(t, s, ctx, 100, nativeWorkerAddress)
	tx, err := s.pool.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if ok, err := staking.Publish(ctx, tx, &staking.Snapshot{
		ChainID: s.chainID, Address: nativeWorkerAddress, Height: 100, Bonded: "0", Unbonding: "0",
	}); err != nil || !ok {
		t.Fatalf("initial evidence: %v %v", ok, err)
	}
	bundle := &BlockBundle{ChainID: s.chainID, Height: 101, BlockTime: time.Now().UTC(), BlockHashHex: "fixture",
		FinalizeBlockEvents: []rpc.Event{{Type: "complete_unbonding", Attributes: []rpc.Attribute{
			{Key: "amount", Value: ""}, {Key: "delegator", Value: nativeWorkerAddress},
			{Key: "validator", Value: "structsvaloper1pa4ch99mktue309avws5xxp70t4rr5egxfmcmp"},
		}}}}
	result, err := s.applyBlockInTx(ctx, tx, bundle, 101, buffers.New(), applyOpts{DeferProjections: true})
	if err != nil {
		t.Fatal(err)
	}
	var pending bool
	var required int64
	if err := tx.QueryRow(ctx, `SELECT pending,required_height FROM sync_state.native_staking_snapshot
WHERE chain_id=$1 AND address=$2`, s.chainID, nativeWorkerAddress).Scan(&pending, &required); err != nil || !pending || required != 101 {
		t.Fatalf("empty completion did not invalidate: %v %d %v", pending, required, err)
	}
	if _, ok := result.Dirty.Addresses[nativeWorkerAddress]; !ok || len(result.LedgerDeltas) != 0 {
		t.Fatal("completion must dirty the claim without inventing financial legs")
	}
}

func TestNativeWorker_StopCancelsActiveHTTP(t *testing.T) {
	s, ctx := nativeWorkerFixture(t)
	nativeRequest(t, s, ctx, 100, nativeWorkerAddress)
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/delegations/") {
			close(entered)
			<-r.Context().Done()
			return
		}
		emptyNativeResponse(w, r, s.chainID)
	}))
	t.Cleanup(server.Close)
	s.cfg.LCDURL = server.URL
	stop := s.startNativeRefresh(ctx)
	select {
	case <-entered:
	case <-ctx.Done():
		stop()
		t.Fatal("worker never fetched")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not join after cancellation")
	}
}
