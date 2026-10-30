package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/protocol"
)

func TestEconomyCoverage_RequiresContiguousActivation(t *testing.T) {
	pool, ctx := connectPool(t), context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	const chain = "structstestnet-111"
	const h = protocol.V022TestnetHeight
	if _, err := tx.Exec(ctx, `DELETE FROM sync_state.economy_contract WHERE chain_id=$1`, chain); err != nil {
		t.Fatal(err)
	}
	cursor := Cursor{ChainID: chain, LastHeight: h, LastBlockHash: strings.Repeat("A", 64)}
	if err := RequireEconomyCoverage(ctx, tx, cursor); err == nil {
		t.Fatal("legacy cursor counted as new event coverage")
	}
	if err := RecordEconomyCoverage(ctx, tx, chain, h+1, strings.Repeat("B", 64)); err == nil {
		t.Fatal("skipped activation")
	} else if !strings.Contains(err.Error(), "missing contiguous") {
		t.Fatal(err)
	}
	if err := RecordEconomyCoverage(ctx, tx, chain, h, cursor.LastBlockHash); err != nil {
		t.Fatal(err)
	}
	if err := RequireEconomyCoverage(ctx, tx, cursor); err != nil {
		t.Fatal(err)
	}
	if err := RecordEconomyCoverage(ctx, tx, chain, h, cursor.LastBlockHash); err != nil {
		t.Fatal("same-block retry:", err)
	}
	if err := RecordEconomyCoverage(ctx, tx, chain, h, strings.Repeat("B", 64)); err == nil {
		t.Fatal("changed hash at the same height")
	}
	if err := RecordEconomyCoverage(ctx, tx, chain, h+2, strings.Repeat("C", 64)); err == nil {
		t.Fatal("jumped a coverage gap")
	}
	if err := RecordEconomyCoverage(ctx, tx, chain, h+1, strings.Repeat("B", 64)); err != nil {
		t.Fatal(err)
	}
	if err := RequireEconomyCoverage(ctx, tx, cursor); err == nil {
		t.Fatal("accepted coverage at a different cursor height")
	}
	if err := RecordEconomyCoverage(ctx, tx, chain, h, strings.Repeat("A", 64)); err == nil {
		t.Fatal("rewound coverage over a newer boundary")
	}
	cursor.LastHeight, cursor.LastBlockHash = h+1, strings.Repeat("B", 64)
	if err := RequireEconomyCoverage(ctx, tx, cursor); err != nil {
		t.Fatal(err)
	}
}
