package db

import (
	"context"
	"strings"
	"testing"
)

func TestEconomySchema_RequiresSqitchContract(t *testing.T) {
	pool, ctx := connectPool(t), context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := CheckEconomySchema(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE structs_indexer`); err != nil {
		t.Fatal(err)
	}
	if err := CheckEconomySchema(ctx, tx); err != nil {
		t.Fatalf("indexer role: %v", err)
	}
	if _, err := tx.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sync_state.infusion_event_position DROP COLUMN phase`); err != nil {
		t.Fatal(err)
	}
	if err := CheckEconomySchema(ctx, tx); err == nil || !strings.Contains(err.Error(), "infusion_event_position.phase") {
		t.Fatalf("missing migration accepted: %v", err)
	}
}

func TestEconomySchema_RejectsCoercingNumeric(t *testing.T) {
	pool, ctx := connectPool(t), context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `ALTER TABLE structs.owner_reactor_diversity ALTER COLUMN power TYPE numeric(78,0)`); err != nil {
		t.Fatal(err)
	}
	if err := CheckEconomySchema(ctx, tx); err == nil || !strings.Contains(err.Error(), "without numeric coercion") {
		t.Fatalf("coercing numeric accepted: %v", err)
	}
}
