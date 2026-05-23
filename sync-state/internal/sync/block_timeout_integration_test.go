package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Integration test for the per-block SET LOCAL statement_timeout
// behaviour. We don't run a full applyBlock here — we just stand up a
// transaction the way applyBlock does (SET LOCAL statement_timeout =
// 500ms) and confirm that a pg_sleep that runs longer than that gets
// killed with SQLSTATE 57014 (query_canceled).
//
// Set INTEGRATION_DATABASE_URL to opt in.
//
//	INTEGRATION_DATABASE_URL=postgres://structs@localhost:5432/structs?sslmode=disable \
//	    go test ./internal/sync -run TestStatementTimeout -v
func TestStatementTimeout_KillsHungQuery(t *testing.T) {
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	defer conn.Close(context.Background())

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())

	// Mirror applyBlock's prelude exactly.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", (500*time.Millisecond).Milliseconds())); err != nil {
		t.Fatalf("set statement_timeout: %v", err)
	}

	start := time.Now()
	_, err = tx.Exec(ctx, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, query slept for %s and returned nil", elapsed)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "57014" {
		t.Fatalf("expected SQLSTATE 57014 (query_canceled), got %s: %v", pgErr.Code, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("statement_timeout=500ms but query ran for %s", elapsed)
	}
	if !strings.Contains(strings.ToLower(pgErr.Message), "statement timeout") &&
		!strings.Contains(strings.ToLower(pgErr.Message), "canceling statement") {
		t.Fatalf("unexpected message for timeout: %q", pgErr.Message)
	}
}

