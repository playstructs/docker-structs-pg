package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
		_, _ = pool.Exec(context.Background(), `DELETE FROM structs.api_inventory WHERE owner_type='address' AND owner_id=ANY($1::varchar[])`, []string{nativeWorkerAddress, nativeWorkerOther, "structs1qqqqqqqq", "structs1pppppppp"})
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
	if _, err := tx.Exec(ctx, `INSERT INTO structs.current_block(chain,height) VALUES($1,100)`, s.chainID); err != nil {
		t.Fatal(err)
	}
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

func TestNativeWorker_PublishesClaimsAndNotificationsAtomically(t *testing.T) {
	if os.Getenv("INTEGRATION_MAINTENANCE_TESTS") != "1" {
		t.Skip("INTEGRATION_MAINTENANCE_TESTS=1 requires a disposable database")
	}
	s, ctx := nativeWorkerFixture(t)
	const player = "1-996301"
	var prior string
	if err := s.pool.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(r),'[]')::text FROM structs.api_refresh_state r`).Scan(&prior); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		tx, err := s.pool.Pool.Begin(cleanup)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(cleanup)
		for _, sql := range []string{
			`DELETE FROM structs.api_inventory WHERE owner_id IN ('1-996301','` + nativeWorkerAddress + `')`,
			`DELETE FROM structs.api_leaderboard_player WHERE player_id='1-996301'`,
			`DELETE FROM structs.player_address WHERE player_id='1-996301'`,
			`DELETE FROM structs.grid WHERE object_id='1-996301'`,
			`DELETE FROM structs.player WHERE id='1-996301'`,
			`DELETE FROM structs.api_work_dirty WHERE id='1-996301'`,
			`DELETE FROM structs.api_refresh_state`,
		} {
			if _, err := tx.Exec(cleanup, sql); err != nil {
				t.Error(err)
				return
			}
		}
		if _, err := tx.Exec(cleanup, `INSERT INTO structs.api_refresh_state SELECT * FROM jsonb_populate_recordset(NULL::structs.api_refresh_state,$1::jsonb)`, prior); err != nil {
			t.Error(err)
			return
		}
		if err := tx.Commit(cleanup); err != nil {
			t.Error(err)
		}
	})
	seed, err := s.pool.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(ctx)
	if _, err := seed.Exec(ctx, `INSERT INTO structs.player(id,primary_address) VALUES($1,$2)`, player, nativeWorkerAddress); err != nil {
		t.Fatal(err)
	}
	dirty := readmodel.NewDirty()
	dirty.Address(nativeWorkerAddress)
	if err := readmodel.Recompute(ctx, seed, dirty, 77, time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), readmodel.InventoryFromLedger()); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	nativeRequest(t, s, ctx, 100, nativeWorkerAddress)
	var before string
	if err := s.pool.Pool.QueryRow(ctx, `SELECT jsonb_agg(r ORDER BY model)::text FROM structs.api_refresh_state r`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	listener, err := pgx.Connect(ctx, os.Getenv("INTEGRATION_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(context.Background())
	if _, err := listener.Exec(ctx, `LISTEN grass`); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emptyNativeResponse(w, r, s.chainID)
	}))
	defer server.Close()
	s.cfg.LCDURL = server.URL
	if n, err := s.refreshNativeBatch(ctx); err != nil || n != 1 {
		t.Fatalf("publication count=%d err=%v", n, err)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		n, err := listener.WaitForNotification(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(n.Payload), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["subject"] != "structs.grid.player."+player || payload["invalidates"] != "inventory" {
			t.Fatalf("unexpected claim notification: %s", n.Payload)
		}
		seen[payload["denom"]] = true
		var ready bool
		if err := s.pool.Pool.QueryRow(ctx, `SELECT alpha_value=0 AND alpha_balance=0 FROM structs.api_leaderboard_player WHERE player_id=$1`, player).Scan(&ready); err != nil || !ready {
			t.Fatalf("notification preceded committed projection: ready=%v err=%v", ready, err)
		}
	}
	if !seen["ualpha.infused"] || !seen["ualpha.defusing"] {
		t.Fatalf("missing claim invalidation: %v", seen)
	}
	var after string
	if err := s.pool.Pool.QueryRow(ctx, `SELECT jsonb_agg(r ORDER BY model)::text FROM structs.api_refresh_state r`).Scan(&after); err != nil || after != before {
		t.Fatalf("worker advanced incomplete projection boundary: %s != %s err=%v", after, before, err)
	}
	// A failed publication transaction must expose neither changed totals
	// nor a notification that claims they were committed.
	tx, err := s.pool.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE sync_state.native_staking_snapshot SET bonded=9 WHERE chain_id=$1`, s.chainID); err != nil {
		t.Fatal(err)
	}
	if err := readmodel.RefreshNativeClaims(ctx, tx, nativeWorkerAddress); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if n, err := listener.WaitForNotification(waitCtx); err == nil || n != nil {
		t.Fatalf("rolled-back claim notified: %v %v", n, err)
	}
	var unchanged bool
	if err := s.pool.Pool.QueryRow(ctx, `SELECT alpha_value=0 FROM structs.api_leaderboard_player WHERE player_id=$1`, player).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("rolled-back value escaped: unchanged=%v err=%v", unchanged, err)
	}
}

func TestNativeWorker_BoundedParallelFetch(t *testing.T) {
	s, ctx := nativeWorkerFixture(t)
	s.cfg.NativeStakingParallelism = 2
	addresses := []string{nativeWorkerAddress, nativeWorkerOther, "structs1qqqqqqqq", "structs1pppppppp"}
	nativeRequest(t, s, ctx, 100, addresses...)
	entered, release := make(chan struct{}, 8), make(chan struct{})
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/delegations/") {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		emptyNativeResponse(w, r, s.chainID)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	s.cfg.LCDURL = server.URL
	done := make(chan error, 1)
	go func() {
		n, err := s.refreshNativeBatch(ctx)
		if err == nil && n != len(addresses) {
			err = fmt.Errorf("published %d", n)
		}
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("fetches did not run concurrently")
		}
	}
	select {
	case <-entered:
		t.Fatal("exceeded concurrency limit")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency=%d", peak.Load())
	}
}

func TestNativeParallelismBounds(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 4}, {-1, 4}, {1, 1}, {4, 4}, {8, 8}, {99, 8}} {
		if got := nativeParallelism(tc.in); got != tc.want {
			t.Fatalf("%d -> %d, want %d", tc.in, got, tc.want)
		}
	}
}
