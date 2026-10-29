package verify

import (
	"context"
	"testing"
	"time"

	"sync-state/internal/readmodel"
)

func TestCheckAPI_CurrentClaimsAndUnknownValue(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	const chain, player, address = "verify-claims", "1-996201", "verify-claims-address"
	stamp := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`DELETE FROM sync_state.sync_cursor`)
	exec(`INSERT INTO sync_state.sync_cursor(chain_id,last_height,last_block_time) VALUES($1,100,$2)`, chain, stamp)
	exec(`INSERT INTO structs.player(id,primary_address) VALUES($1,$2)`, player, address)
	exec(`INSERT INTO structs.ledger(address,denom,amount_p,direction,action,block_height)
VALUES ($1,'ualpha',10,'credit','received',100),($1,'ualpha.infused',999,'credit','infused',100)`, address)
	d := readmodel.NewDirty()
	d.Address(address)
	refresh := func() {
		t.Helper()
		if err := readmodel.Recompute(ctx, tx, d, 100, stamp, readmodel.InventoryFromLedger()); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want Status) {
		t.Helper()
		r := checkAPIReadModels(ctx, Inputs{Pool: tx, ChainID: chain})
		if r.Status != want {
			t.Fatalf("got=%s want=%s detail=%s counts=%v", r.Status, want, r.Detail, r.Counts)
		}
	}
	refresh()
	check(StatusPass) // Unknown is a correct projection, not fabricated zero.
	exec(`UPDATE structs.api_leaderboard_player SET alpha_value=10 WHERE player_id=$1`, player)
	check(StatusFail)
	refresh()
	exec(`INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height,source_height,bonded,unbonding,fetched_at)
VALUES ($1,$2,100,100,18446744073709551616,7,NOW())`, chain, address)
	refresh()
	check(StatusPass)
	exec(`UPDATE structs.api_inventory SET balance=999 WHERE owner_id=$1 AND denom='ualpha.infused'`, address)
	check(StatusFail)
	refresh()
	exec(`UPDATE structs.api_refresh_state SET inventory_authority='ledger' WHERE model='inventory'`)
	check(StatusFail)
}
