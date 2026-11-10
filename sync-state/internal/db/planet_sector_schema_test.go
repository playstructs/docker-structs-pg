package db

import (
	"context"
	"strings"
	"testing"
)

func TestEconomySchema_PlanetSectorRequired(t *testing.T) {
	pool, ctx := connectPool(t), context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// This disposable transaction models starting the new writer on old schema.
	if _, err := tx.Exec(ctx, `DROP VIEW view.planet; ALTER TABLE structs.planet DROP COLUMN sector`); err != nil {
		t.Fatal(err)
	}
	if err := CheckEconomySchema(ctx, tx); err == nil || !strings.Contains(err.Error(), "structs.planet.sector") {
		t.Fatalf("missing sector migration accepted: %v", err)
	}
}
