// Integration coverage for `sync-state reprocess-errors`. The fake-RPC
// harness mints a block containing a known EventGrid attribute; we seed
// a handler_error_log row pointing at it, run reprocess.Run, and assert:
//
//   - the row was marked resolved (or "would-resolve" in dry-run)
//   - the registered gridHandler actually executed (we inspect structs.grid)
//   - dry-run does NOT touch the row's resolved_at column
//
// Skips silently when INTEGRATION_DATABASE_URL is unset so this stays a
// no-op on machines without a local Postgres.
package reprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/rpc"
)

// fakeRPC stitches together the minimum CometBFT surface reprocess.Run
// hits: /status (chain_id), /block (header), /block_results (events).
type fakeRPC struct {
	t       *testing.T
	server  *httptest.Server
	chainID string
	height  int64
	events  []rpc.Event
}

func newFakeRPC(t *testing.T, chainID string, height int64, evs []rpc.Event) *fakeRPC {
	t.Helper()
	f := &fakeRPC{
		t:       t,
		chainID: chainID,
		height:  height,
		events:  evs,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(func() { f.server.Close() })
	return f
}

func (f *fakeRPC) URL() string { return f.server.URL }

func (f *fakeRPC) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/status"):
		body := fmt.Sprintf(`{
			"node_info":{"network":%q},
			"sync_info":{
				"latest_block_height":%q,
				"earliest_block_height":"1",
				"latest_block_time":"2026-05-20T00:00:00Z",
				"catching_up":false
			}
		}`, f.chainID, strconv.FormatInt(f.height, 10))
		writeEnv(w, json.RawMessage(body))
	case strings.HasPrefix(r.URL.Path, "/block_results"):
		evsJSON, _ := json.Marshal(f.events)
		body := fmt.Sprintf(`{"height":%q,"txs_results":[],"finalize_block_events":%s}`,
			strconv.FormatInt(f.height, 10), string(evsJSON))
		writeEnv(w, json.RawMessage(body))
	case strings.HasPrefix(r.URL.Path, "/block"):
		body := fmt.Sprintf(`{
			"block_id":{"hash":"AAAA"},
			"block":{
				"header":{"height":%q,"chain_id":%q,"time":"2026-05-20T00:00:00Z","proposer_address":"BB"},
				"data":{"txs":[]}
			}
		}`, strconv.FormatInt(f.height, 10), f.chainID)
		writeEnv(w, json.RawMessage(body))
	default:
		http.NotFound(w, r)
	}
}

func writeEnv(w http.ResponseWriter, result json.RawMessage) {
	_ = json.NewEncoder(w).Encode(rpc.Envelope{JSONRPC: "2.0", ID: json.RawMessage(`-1`), Result: result})
}

func connectPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	if err := db.Bootstrap(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedErrorRow(t *testing.T, pool *pgxpool.Pool, chain, ck string, height int64) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO sync_state.handler_error_log
			(chain_id, height, tx_index, msg_index, event_index, composite_key, error, severity)
		VALUES ($1, $2, -1, -1, 0, $3, 'synthetic seed for reprocess test', 'error')
		RETURNING id
	`, chain, height, ck).Scan(&id)
	if err != nil {
		t.Fatalf("seed err row: %v", err)
	}
	return id
}

// gridEvent builds a rpc.Event payload for EventGrid with a single
// attribute named "gridRecord". The value is a JSON object with the
// attributeId and value the gridHandler will parse.
func gridEvent(attributeID, value string) rpc.Event {
	body, _ := json.Marshal(map[string]string{"attributeId": attributeID, "value": value})
	return rpc.Event{
		Type: "structs.structs.EventGrid",
		Attributes: []rpc.Attribute{
			{Key: "gridRecord", Value: string(body)},
		},
	}
}

func TestReprocess_ResolvesValidGridEvent(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := "rep-test-resolves-" + t.Name()
	ck := "structs.structs.EventGrid.gridRecord"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
		_, _ = pool.Exec(ctx, `DELETE FROM structs.grid WHERE id = '0-5-91123'`)
	})

	id := seedErrorRow(t, pool, chain, ck, 7)
	fake := newFakeRPC(t, chain, 7, []rpc.Event{
		gridEvent("0-5-91123", "1000"),
	})
	client := rpc.NewClient([]string{fake.URL()}, 5*time.Second, 0)

	router := events.NewRouter(false)
	var stdout, stderr bytes.Buffer
	code := Run(ctx, CmdInputs{
		Pool:    pool,
		RPC:     client,
		Router:  router,
		ChainID: chain,
		Limit:   100,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "RESOLVED") {
		t.Errorf("stdout did not mark RESOLVED:\n%s", stdout.String())
	}

	// Row should be marked resolved.
	var resolvedBy *string
	err := pool.QueryRow(ctx, `SELECT resolved_by FROM sync_state.handler_error_log WHERE id = $1`, id).Scan(&resolvedBy)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if resolvedBy == nil || *resolvedBy != "reprocess-errors" {
		t.Errorf("resolved_by = %v want reprocess-errors", resolvedBy)
	}

	// gridHandler should have written the row.
	var val int64
	err = pool.QueryRow(ctx, `SELECT val FROM structs.grid WHERE id = '0-5-91123'`).Scan(&val)
	if err != nil {
		t.Fatalf("read grid: %v", err)
	}
	if val != 1000 {
		t.Errorf("grid.val = %d want 1000", val)
	}
}

func TestReprocess_DryRunDoesNotResolve(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := "rep-test-dryrun-" + t.Name()
	ck := "structs.structs.EventGrid.gridRecord"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
		_, _ = pool.Exec(ctx, `DELETE FROM structs.grid WHERE id = '0-5-91124'`)
	})

	id := seedErrorRow(t, pool, chain, ck, 8)
	fake := newFakeRPC(t, chain, 8, []rpc.Event{
		gridEvent("0-5-91124", "777"),
	})
	client := rpc.NewClient([]string{fake.URL()}, 5*time.Second, 0)

	router := events.NewRouter(false)
	var stdout, stderr bytes.Buffer
	code := Run(ctx, CmdInputs{
		Pool:    pool,
		RPC:     client,
		Router:  router,
		ChainID: chain,
		Limit:   100,
		DryRun:  true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "WOULD-RESOLVE") {
		t.Errorf("stdout missing WOULD-RESOLVE:\n%s", stdout.String())
	}

	// Row should still be unresolved.
	var resolvedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT resolved_at FROM sync_state.handler_error_log WHERE id = $1`, id).Scan(&resolvedAt)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if resolvedAt != nil {
		t.Errorf("dry-run resolved the row: %v", resolvedAt)
	}

	// And grid row should NOT have been written (tx rolled back).
	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM structs.grid WHERE id = '0-5-91124'`).Scan(&count)
	if count != 0 {
		t.Errorf("dry-run left grid row behind: %d rows", count)
	}
}

func TestReprocess_NoMatchingRows(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := "rep-test-empty-" + t.Name()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
	})

	fake := newFakeRPC(t, chain, 1, nil)
	client := rpc.NewClient([]string{fake.URL()}, 5*time.Second, 0)
	router := events.NewRouter(false)

	var stdout, stderr bytes.Buffer
	code := Run(ctx, CmdInputs{
		Pool:    pool,
		RPC:     client,
		Router:  router,
		ChainID: chain,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout.String(), "No matching unresolved") {
		t.Errorf("stdout = %q", stdout.String())
	}
}
