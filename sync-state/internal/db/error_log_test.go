// Integration coverage for sync_state.handler_error_log helpers:
// WriteHandlerError severity round-trip, UnresolvedErrorSummary tallying,
// ListUnresolvedErrors filtering, and ResolveErrorByID resolution.
//
// Skips silently when INTEGRATION_DATABASE_URL is not set so `go test ./...`
// stays green on machines without a local Postgres.
package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	if err := Bootstrap(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// uniqueChainID gives each test its own chain_id slice so the tests don't
// step on each other when run in parallel against a shared dev DB.
func uniqueChainID(t *testing.T) string {
	t.Helper()
	return "errlog-test-" + t.Name()
}

func TestErrorLog_SeverityRoundTrip(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := uniqueChainID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
	})

	cases := []struct {
		name string
		sev  string
		want string
	}{
		{"empty defaults to error", "", "error"},
		{"explicit error", "error", "error"},
		{"explicit warn", "warn", "warn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]string{"k": "v"})
			err := WriteHandlerError(ctx, pool, HandlerError{
				ChainID:      chain,
				Height:       1,
				CompositeKey: "test.composite." + c.name,
				Payload:      payload,
				Error:        "synthetic",
				Severity:     c.sev,
			})
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			var got string
			err = pool.QueryRow(ctx, `
				SELECT severity FROM sync_state.handler_error_log
				 WHERE chain_id = $1 AND composite_key = $2
				 ORDER BY id DESC LIMIT 1
			`, chain, "test.composite."+c.name).Scan(&got)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got != c.want {
				t.Errorf("severity = %q want %q", got, c.want)
			}
		})
	}
}

func TestErrorLog_UnresolvedErrorSummary(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := uniqueChainID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
	})

	// 2 errors, 3 warns, 1 resolved.
	mustWrite := func(sev string) int64 {
		t.Helper()
		var id int64
		// Use a transaction so we can capture the inserted id easily.
		err := pool.QueryRow(ctx, `
			INSERT INTO sync_state.handler_error_log
				(chain_id, height, composite_key, error, severity)
			VALUES ($1, 1, 'ck', 'syn', $2)
			RETURNING id
		`, chain, sev).Scan(&id)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return id
	}
	mustWrite("error")
	mustWrite("error")
	mustWrite("warn")
	mustWrite("warn")
	mustWrite("warn")
	resolvedID := mustWrite("error")
	if _, err := pool.Exec(ctx, `
		UPDATE sync_state.handler_error_log
		   SET resolved_at = NOW(), resolved_by = 'test'
		 WHERE id = $1
	`, resolvedID); err != nil {
		t.Fatalf("pre-resolve: %v", err)
	}

	errCount, warnCount, err := UnresolvedErrorSummary(ctx, pool, chain)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if errCount != 2 {
		t.Errorf("errCount = %d want 2", errCount)
	}
	if warnCount != 3 {
		t.Errorf("warnCount = %d want 3", warnCount)
	}
}

func TestErrorLog_ListAndResolve(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	chain := uniqueChainID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id = $1`, chain)
	})

	seed := func(height int64, ck, sev string) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO sync_state.handler_error_log
				(chain_id, height, composite_key, error, severity)
			VALUES ($1, $2, $3, 'syn', $4) RETURNING id
		`, chain, height, ck, sev).Scan(&id)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return id
	}
	a := seed(10, "structs.structs.EventGrid.gridRecord", "warn")
	b := seed(20, "structs.structs.EventGrid.gridRecord", "error")
	c := seed(30, "structs.structs.EventPlanet.planet", "error")

	// composite-key filter
	rows, err := ListUnresolvedErrors(ctx, pool, chain,
		"structs.structs.EventGrid.gridRecord", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("composite filter rows = %d want 2", len(rows))
	}
	if rows[0].ID != a || rows[1].ID != b {
		t.Errorf("order: got [%d,%d] want [%d,%d]", rows[0].ID, rows[1].ID, a, b)
	}

	// severity filter
	rows, err = ListUnresolvedErrors(ctx, pool, chain, "", "warn", 0, 0, 0)
	if err != nil {
		t.Fatalf("sev list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != a {
		t.Errorf("warn filter rows = %v", rows)
	}

	// height range
	rows, err = ListUnresolvedErrors(ctx, pool, chain, "", "", 15, 25, 0)
	if err != nil {
		t.Fatalf("range list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != b {
		t.Errorf("range filter rows = %v", rows)
	}

	// limit
	rows, err = ListUnresolvedErrors(ctx, pool, chain, "", "", 0, 0, 2)
	if err != nil {
		t.Fatalf("limit list: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("limit rows = %d want 2", len(rows))
	}

	// resolve b, ensure list shrinks
	n, err := ResolveErrorByID(ctx, pool, b, "test")
	if err != nil || n != 1 {
		t.Fatalf("resolve: n=%d err=%v", n, err)
	}
	// Re-resolving is a no-op (returns 0).
	n2, err := ResolveErrorByID(ctx, pool, b, "test")
	if err != nil || n2 != 0 {
		t.Errorf("idempotent resolve: n=%d err=%v", n2, err)
	}

	rows, err = ListUnresolvedErrors(ctx, pool, chain, "", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("post-resolve list: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("post-resolve rows = %d want 2 (only %d and %d remain)", len(rows), a, c)
	}
}

// Helper: every-test smoke that Bootstrap is idempotent on top of itself.
func TestBootstrap_IdempotentSeverity(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	// Severity column + index must exist.
	var hasCol bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema='sync_state' AND table_name='handler_error_log' AND column_name='severity'
		)
	`).Scan(&hasCol); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if !hasCol {
		t.Fatalf("severity column missing after bootstrap")
	}
	var hasIdx bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			 WHERE schemaname='sync_state' AND indexname='handler_error_log_severity_unresolved_idx'
		)
	`).Scan(&hasIdx); err != nil {
		t.Fatalf("idx introspect: %v", err)
	}
	if !hasIdx {
		t.Fatalf("severity index missing after bootstrap")
	}
	_ = pgx.ErrNoRows // keep pgx import in use across builds without other refs
}
