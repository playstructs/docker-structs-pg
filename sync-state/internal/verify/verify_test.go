// Integration coverage for the verify check set. Each test seeds a
// controlled fixture into a per-test chain_id slice (so tests don't
// step on each other), runs the relevant CheckFunc, and asserts the
// classification. Skips when INTEGRATION_DATABASE_URL is unset.
package verify

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/db"
)

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

func uniqueChainID(t *testing.T) string {
	t.Helper()
	return "verify-test-" + t.Name()
}

// runOne is a tiny convenience: run a single CheckFunc with the supplied
// Inputs and return the result. Lets each test stay focused on its slice
// without round-tripping through the runner / report writer.
func runOne(ctx context.Context, fn CheckFunc, in Inputs) CheckResult {
	r := fn(ctx, in)
	r.Name = "test"
	return r
}

func TestCheck_HandlerErrorsUnresolved(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := uniqueChainID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
	})

	in := Inputs{Pool: pool, ChainID: chain}

	// Empty -> PASS
	r := runOne(ctx, checkHandlerErrorsUnresolved, in)
	if r.Status != StatusPass {
		t.Fatalf("empty: status=%s detail=%s", r.Status, r.Detail)
	}

	// Add a warn -> INFO
	_, err := pool.Exec(ctx, `
		INSERT INTO sync_state.handler_error_log
			(chain_id, height, composite_key, error, severity)
		VALUES ($1, 1, 'ck.warn', 'syn', 'warn')
	`, chain)
	if err != nil {
		t.Fatalf("seed warn: %v", err)
	}
	r = runOne(ctx, checkHandlerErrorsUnresolved, in)
	if r.Status != StatusInfo {
		t.Errorf("warn only: status=%s detail=%s", r.Status, r.Detail)
	}

	// Add an error -> FAIL
	_, err = pool.Exec(ctx, `
		INSERT INTO sync_state.handler_error_log
			(chain_id, height, composite_key, error, severity)
		VALUES ($1, 2, 'ck.err', 'syn', 'error')
	`, chain)
	if err != nil {
		t.Fatalf("seed err: %v", err)
	}
	r = runOne(ctx, checkHandlerErrorsUnresolved, in)
	if r.Status != StatusFail {
		t.Errorf("err present: status=%s detail=%s", r.Status, r.Detail)
	}
	// counts is groups + _error_total + _warn_total.
	if r.Counts["_error_total"] != int64(1) || r.Counts["_warn_total"] != int64(1) {
		t.Errorf("counts off: %+v", r.Counts)
	}
}

func TestCheck_BlockLogCoverage(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := uniqueChainID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.block_log WHERE chain_id = $1`, chain)
	})
	in := Inputs{Pool: pool, ChainID: chain}

	// Empty -> INFO
	r := runOne(ctx, checkBlockLogCoverage, in)
	if r.Status != StatusInfo {
		t.Errorf("empty: status=%s", r.Status)
	}

	// Contiguous 1..5 -> PASS
	for h := int64(1); h <= 5; h++ {
		_, err := pool.Exec(ctx, `
			INSERT INTO sync_state.block_log
				(chain_id, height, block_hash, block_time, num_txs, num_events, num_handler_errors)
			VALUES ($1, $2, 'hash', NOW(), 0, 0, 0)
		`, chain, h)
		if err != nil {
			t.Fatalf("seed h=%d: %v", h, err)
		}
	}
	r = runOne(ctx, checkBlockLogCoverage, in)
	if r.Status != StatusPass {
		t.Errorf("contiguous: status=%s detail=%s", r.Status, r.Detail)
	}

	// Punch a gap at h=3 (delete it) -> FAIL
	if _, err := pool.Exec(ctx, `DELETE FROM sync_state.block_log WHERE chain_id=$1 AND height=3`, chain); err != nil {
		t.Fatalf("delete h=3: %v", err)
	}
	r = runOne(ctx, checkBlockLogCoverage, in)
	if r.Status != StatusFail {
		t.Errorf("gap: status=%s detail=%s", r.Status, r.Detail)
	}
	if r.Counts["missing_blocks"] != int64(1) {
		t.Errorf("missing_blocks = %v want 1", r.Counts["missing_blocks"])
	}
}

func TestCheck_RawMirrorCoverage_SkipWhenOff(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	in := Inputs{Pool: pool, ChainID: "anything", MirrorRaw: false}
	r := runOne(ctx, checkRawMirrorCoverage, in)
	if r.Status != StatusSkip {
		t.Errorf("mirror off: status=%s", r.Status)
	}
}

func TestRunChecks_WritesReport(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := uniqueChainID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.verification_report`)
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
	})
	in := Inputs{Pool: pool, ChainID: chain}

	results, err := RunChecks(ctx, in, Options{WriteReport: true})
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM sync_state.verification_report`).Scan(&rowCount); err != nil {
		t.Fatalf("count report: %v", err)
	}
	if rowCount != len(results) {
		t.Errorf("verification_report rows=%d want %d", rowCount, len(results))
	}
}

func TestRunChecks_ErrorsOnlyRunsOneCheck(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := uniqueChainID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.verification_report`)
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
	})
	in := Inputs{Pool: pool, ChainID: chain}

	results, err := RunChecks(ctx, in, Options{ErrorsOnly: true, WriteReport: false})
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("errors-only ran %d checks want 1", len(results))
	}
	if len(results) > 0 && results[0].Name != "handler_errors_unresolved" {
		t.Errorf("errors-only ran %q want handler_errors_unresolved", results[0].Name)
	}
}
