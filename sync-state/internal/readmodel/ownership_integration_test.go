package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
)

func projectionBoundary(t *testing.T, ctx context.Context, tx pgx.Tx, height int64) {
	t.Helper()
	for _, sql := range []string{
		`DELETE FROM sync_state.sync_cursor`,
		`DELETE FROM structs.current_block`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_state.sync_cursor(chain_id,last_height,last_block_time)
VALUES ('ownership-test',$1,TIMESTAMPTZ '2026-01-01 00:00:00Z')`, height); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.current_block(chain,height) VALUES ('ownership-test',$1)`, height); err != nil {
		t.Fatal(err)
	}
	if err := refresh(ctx, tx, "inventory", height, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func TestEconomicOwnershipProjectionAndRepair(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		// Exercise the prepared-plan path used by a long-running native worker.
		if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode=force_generic_plan"); err != nil {
			t.Fatal(err)
		}
		const p1, p2, g1, g2 = "1-992101", "1-992102", "0-992101", "0-992102"
		seedIsolatedGuild(t, ctx, tx, g1, "owner-guild-one")
		seedIsolatedGuild(t, ctx, tx, g2, "owner-guild-two")
		seedIsolatedPlayer(t, ctx, tx, p1, "owner-player-one", g1, "")
		seedIsolatedPlayer(t, ctx, tx, p2, "owner-player-two", g2, "")
		addresses := []string{"owner-primary", "owner-retained", "owner-revoked", "owner-pending", "owner-unassociated", "owner-vault", "owner-guild-vault"}
		for i, status := range []string{"approved", "approved", "revoked", "pending"} {
			if _, err := tx.Exec(ctx, `INSERT INTO structs.player_address(address,player_id,status,guild_id) VALUES($1,$2,$3,$4)`, addresses[i], p1, status, g1); err != nil {
				t.Fatal(err)
			}
		}
		var deltas []buffers.LedgerDelta
		for _, a := range addresses {
			seedIsolatedLedger(t, ctx, tx, a, "10", "received", "credit", "ualpha", 100)
			deltas = append(deltas, buffers.LedgerDelta{Address: a, Denom: "ualpha", Delta: "10"})
		}
		d := NewDirty()
		for _, a := range addresses {
			d.Address(a)
		}
		if err := Expand(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		if err := applyInventoryDeltas(ctx, tx, d, deltas); err != nil {
			t.Fatal(err)
		}
		assertBalance := func(kind, owner, want string) {
			t.Helper()
			var got string
			if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance::text FROM structs.api_inventory
WHERE owner_type=$1::structs.object_type AND owner_id=$2 AND denom='ualpha'),'absent')`, kind, owner).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("%s %s balance=%s want %s", kind, owner, got, want)
			}
		}
		assertBalance("player", p1, "20") // Pre-H: only approved addresses.
		assertBalance("guild", g1, "absent")
		if _, err := tx.Exec(ctx, `INSERT INTO structs.vault(owner_id,address,account_number) VALUES($1,$2,0),($3,$4,0)`, p1, addresses[5], g1, addresses[6]); err != nil {
			t.Fatal(err)
		}
		d = NewDirty()
		d.Address(addresses[5])
		d.Address(addresses[6])
		if err := Recompute(ctx, tx, d, 100, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), InventoryFromDeltas(nil)); err != nil {
			t.Fatal(err)
		}
		assertBalance("player", p1, "30")
		assertBalance("guild", g1, "10")
		// Ownership and membership change without a bank event. The old and
		// new owners are both dirtied, as in the address/player handlers.
		if _, err := tx.Exec(ctx, `UPDATE structs.player_address SET player_id=$1 WHERE address=$2`, p2, addresses[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE structs.player SET guild_id=$1 WHERE id=$2`, g2, p1); err != nil {
			t.Fatal(err)
		}
		d = NewDirty()
		d.Player(p1)
		d.Player(p2)
		d.Guild(g1)
		d.Guild(g2)
		if err := Recompute(ctx, tx, d, 101, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), InventoryFromDeltas(nil)); err != nil {
			t.Fatal(err)
		}
		assertBalance("player", p1, "20")
		assertBalance("player", p2, "10")
		assertBalance("guild", g1, "10") // Treasury does not follow a controller.
		for _, a := range addresses {
			assertBalance("address", a, "10")
		}
		snapshot := func() string {
			var result string
			if err := tx.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(owner_type,owner_id,denom,balance) ORDER BY owner_type,owner_id,denom)::text
FROM structs.api_inventory WHERE owner_id=ANY($1::varchar[])`, append(addresses, p1, p2, g1, g2)).Scan(&result); err != nil {
				t.Fatal(err)
			}
			return result
		}
		incremental := snapshot()
		if err := backfillInventory(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if got := snapshot(); got != incremental {
			t.Fatalf("full rebuild differs from incremental:\n%s\n%s", got, incremental)
		}
		projectionBoundary(t, ctx, tx, 101)
		var drift int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_inventory_reconcile(FALSE)`).Scan(&drift); err != nil || drift != 0 {
			t.Fatalf("correct projection drift=%d err=%v", drift, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE structs.api_inventory SET balance=999 WHERE owner_id=ANY($1::varchar[]) AND denom='ualpha'`, []string{addresses[5], p1, g1}); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_inventory_reconcile(TRUE)`).Scan(&drift); err != nil || drift != 3 {
			t.Fatalf("corrupted projection drift=%d err=%v", drift, err)
		}
		// A receipt arrives after the recorded drift. Repair must recompute
		// today's bank balance, not write yesterday's expected value.
		seedIsolatedLedger(t, ctx, tx, addresses[5], "7", "received", "credit", "ualpha", 101)
		if n, err := consumeDriftReady(ctx, tx); err != nil || n != 3 {
			t.Fatalf("repair keys=%d err=%v", n, err)
		}
		assertBalance("address", addresses[5], "17")
		assertBalance("player", p1, "27")
		var leaderboardBalance string
		if err := tx.QueryRow(ctx, `SELECT alpha_balance::text FROM structs.api_leaderboard_player WHERE player_id=$1`, p1).Scan(&leaderboardBalance); err != nil || leaderboardBalance != "27" {
			t.Fatalf("repair left dependent leaderboard stale: balance=%s err=%v", leaderboardBalance, err)
		}
		assertBalance("guild", g1, "10")
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_inventory_reconcile(FALSE)`).Scan(&drift); err != nil || drift != 0 {
			t.Fatalf("repaired projection drift=%d err=%v", drift, err)
		}
	})
}

func TestProjectionMaintenanceSkipsDeferredBoundary(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		projectionBoundary(t, ctx, tx, 100)
		if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=101`); err != nil {
			t.Fatal(err)
		}
		var n int
		for _, sql := range []string{
			`SELECT count(*) FROM structs.api_projection_boundary()`,
			`SELECT count(*) FROM structs.api_inventory_reconcile(TRUE)`,
			`SELECT count(*) FROM structs.api_work_refresh_scheduled()`,
		} {
			if err := tx.QueryRow(ctx, sql).Scan(&n); err != nil || n != 0 {
				t.Fatalf("deferred maintenance returned %d: %s: %v", n, sql, err)
			}
		}
	})
}

func TestFullProjectionRefreshRepairsUnqueuedWork(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO structs.api_work(object_id,target_id,category,source_height)
VALUES ('5-992199','5-992199','MINE',99)`); err != nil {
			t.Fatal(err)
		}
		if err := Invalidate(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if err := recompute(ctx, tx, NewDirty(), 101, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), InventoryFromLedger(), true); err != nil {
			t.Fatal(err)
		}
		var stale int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_work WHERE object_id='5-992199'`).Scan(&stale); err != nil || stale != 0 {
			t.Fatalf("full refresh retained unqueued orphan work: count=%d err=%v", stale, err)
		}
		var ready int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_refresh_state WHERE source_height=101`).Scan(&ready); err != nil || ready != len(modelNames) {
			t.Fatalf("full refresh incomplete: models=%d err=%v", ready, err)
		}
	})
}

func TestMaintenanceFunctionsAsIndexer(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		projectionBoundary(t, ctx, tx, 100)
		if _, err := tx.Exec(ctx, `INSERT INTO structs.api_work_dirty(kind,id) VALUES('player','1-995199') ON CONFLICT DO NOTHING`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE structs_indexer`); err != nil {
			t.Fatal(err)
		}
		var superuser bool
		if err := tx.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&superuser); err != nil || superuser {
			t.Fatalf("maintenance fixture requires a non-superuser indexer: superuser=%v err=%v", superuser, err)
		}
		for _, query := range []string{
			`SELECT count(*) FROM structs.api_projection_boundary()`,
			`SELECT count(*) FROM structs.api_work_refresh(100,TIMESTAMPTZ '2026-01-01 00:00:00Z')`,
			`SELECT count(*) FROM structs.api_work_refresh_scheduled()`,
			`SELECT count(*) FROM structs.api_work_reconcile(TRUE)`,
			`SELECT count(*) FROM structs.api_inventory_reconcile(TRUE)`,
		} {
			var n int
			if err := tx.QueryRow(ctx, query).Scan(&n); err != nil {
				t.Fatalf("non-superuser maintenance failed: %s: %v", query, err)
			}
		}
	})
}
