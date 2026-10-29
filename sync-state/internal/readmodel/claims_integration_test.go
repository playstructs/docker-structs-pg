package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
)

func TestCurrentClaimsLifecycleAndAllRepairPaths(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		const p1, p2, guild = "1-996101", "1-996102", "0-996101"
		const vault, retained = "claims-vault", "claims-retained"
		const bonded = "18446744073709551616000"
		seedIsolatedGuild(t, ctx, tx, guild, "claims-guild")
		seedIsolatedPlayer(t, ctx, tx, p1, "claims-one", guild, "")
		seedIsolatedPlayer(t, ctx, tx, p2, "claims-two", guild, "")
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				t.Fatal(err)
			}
		}
		exec(`INSERT INTO structs.vault(owner_id,address,account_number) VALUES($1,$2,0)`, p1, vault)
		exec(`INSERT INTO structs.player_address(address,player_id,status) VALUES($1,$2,'approved')`, retained, p1)
		projectionBoundary(t, ctx, tx, 100)
		seedIsolatedLedger(t, ctx, tx, vault, "1000", "received", "credit", "ualpha", 100)
		seedIsolatedLedger(t, ctx, tx, retained, "50", "received", "credit", "ualpha", 100)
		// Historical receipts deliberately disagree with current positions.
		seedIsolatedLedger(t, ctx, tx, vault, "999", "infused", "credit", "ualpha.infused", 100)
		seedIsolatedLedger(t, ctx, tx, vault, "888", "defusion_started", "credit", "ualpha.defusing", 100)
		exec(`INSERT INTO structs.infusion(destination_id,address,destination_type,player_id,fuel_p,defusing_p,generator_disabled)
VALUES ('5-996101',$1,'struct',$2,25,0,TRUE),('3-996101',$1,'reactor',$2,999,888,FALSE)`, vault, p1)
		d := NewDirty()
		d.Address(vault)
		d.Address(retained)
		d.Player(p1)
		d.Player(p2)
		stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		height := int64(100)
		recompute := func(plan InventoryPlan) {
			t.Helper()
			if err := Recompute(ctx, tx, d, height, stamp, plan); err != nil {
				t.Fatal(err)
			}
		}
		balance := func(kind, owner, denom string, want *string) {
			t.Helper()
			var got *string
			if err := tx.QueryRow(ctx, `SELECT balance::text FROM structs.api_inventory
WHERE owner_type=$1::structs.object_type AND owner_id=$2 AND denom=$3`, kind, owner, denom).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (want == nil) || (got != nil && *got != *want) {
				t.Fatalf("%s %s %s: got=%v want=%v", kind, owner, denom, got, want)
			}
		}
		value := func(want *string) {
			t.Helper()
			var got *string
			if err := tx.QueryRow(ctx, `SELECT alpha_value::text FROM structs.api_leaderboard_player WHERE player_id=$1`, p1).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (want == nil) || (got != nil && *got != *want) {
				t.Fatalf("player value got=%v want=%v", got, want)
			}
		}
		str := func(v string) *string { return &v }
		recompute(InventoryFromDeltas([]buffers.LedgerDelta{
			{Address: vault, Denom: "ualpha", Delta: "1000"},
			{Address: retained, Denom: "ualpha", Delta: "50"},
			{Address: vault, Denom: "ualpha.infused", Delta: "999"},
			{Address: vault, Denom: "ualpha.defusing", Delta: "888"},
		}))
		balance("address", vault, "ualpha", str("1000"))
		balance("player", p1, "ualpha", str("1050"))
		balance("player", p1, "ualpha.infused", nil)
		balance("player", p1, "ualpha.defusing", nil)
		value(nil)
		exec(`INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height,source_height,bonded,unbonding,fetched_at)
VALUES ('ownership-test',$1,100,100,$3,40,NOW()),('ownership-test',$2,100,100,20,5,NOW())`, vault, retained, bonded)
		exec(`INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height,source_height,bonded,unbonding,fetched_at)
SELECT 'ownership-test',address,100,100,0,0,NOW() FROM structs.asset_owner
WHERE owner_id=ANY($1::varchar[]) AND address<>ALL($2::varchar[])`, []string{p1, p2}, []string{vault, retained})
		if err := RefreshNativeClaims(ctx, tx, vault); err != nil {
			t.Fatal(err)
		}
		balance("player", p1, "ualpha.infused", str("18446744073709551616045"))
		balance("player", p1, "ualpha.defusing", str("45"))
		value(str("18446744073709551617140"))
		// Disabled/re-enabled generators retain fuel. Real deletion removes it
		// without creating a refund or disturbing native bonded claims.
		exec(`UPDATE structs.infusion SET generator_disabled=FALSE WHERE destination_id='5-996101'`)
		recompute(InventoryFromDeltas(nil))
		value(str("18446744073709551617140"))
		exec(`DELETE FROM structs.infusion WHERE destination_id='5-996101'`)
		recompute(InventoryFromDeltas(nil))
		value(str("18446744073709551617115"))
		balance("address", vault, "ualpha", str("1000"))
		// A real refund can precede the delayed Structs maturity cleanup.
		// Unknown evidence must not resurrect the old pending ledger total.
		exec(`UPDATE sync_state.native_staking_snapshot SET required_height=101 WHERE address=$1`, vault)
		seedIsolatedLedger(t, ctx, tx, vault, "40", "received", "credit", "ualpha", 101)
		recompute(InventoryFromDeltas([]buffers.LedgerDelta{
			{Address: vault, Denom: "ualpha", Delta: "40"},
			{Address: vault, Denom: "ualpha.defusing", Delta: "-40"},
		}))
		balance("address", vault, "ualpha", str("1040"))
		balance("player", p1, "ualpha.defusing", nil)
		value(nil)
		projectionBoundary(t, ctx, tx, 101)
		height = 101
		exec(`UPDATE sync_state.native_staking_snapshot SET source_height=101,unbonding=0 WHERE address=$1`, vault)
		if err := RefreshNativeClaims(ctx, tx, vault); err != nil {
			t.Fatal(err)
		}
		balance("player", p1, "ualpha.defusing", str("5"))
		value(str("18446744073709551617115"))
		// Delayed Structs cleanup cannot remove the refund or reduce pending twice.
		exec(`UPDATE structs.infusion SET defusing_p=0 WHERE destination_id='3-996101'`)
		recompute(InventoryFromDeltas(nil))
		balance("player", p1, "ualpha.defusing", str("5"))
		value(str("18446744073709551617115"))
		// Native accounting moves stake into an already-funded vault without
		// ordinary delegate receipts. A source slash changes current tokens.
		exec(`UPDATE sync_state.native_staking_snapshot SET bonded=CASE address WHEN $1 THEN bonded+20-7 ELSE 0 END
WHERE address=ANY($2::varchar[])`, vault, []string{vault, retained})
		recompute(InventoryFromDeltas(nil))
		balance("address", retained, "ualpha.infused", str("0"))
		balance("player", p1, "ualpha.infused", str("18446744073709551616013"))
		// Reassigned retained native holdings follow the current address owner.
		exec(`UPDATE structs.player_address SET player_id=$1 WHERE address=$2`, p2, retained)
		recompute(InventoryFromDeltas(nil))
		balance("player", p1, "ualpha.defusing", str("0"))
		balance("player", p2, "ualpha.defusing", str("5"))
		balance("player", p1, "ualpha", str("1040"))
		// Revoking a key drops player attribution, not its blocked native claim.
		exec(`UPDATE structs.player_address SET status='revoked' WHERE address=$1`, retained)
		recompute(InventoryFromDeltas(nil))
		balance("address", retained, "ualpha.defusing", str("5"))
		balance("player", p2, "ualpha.defusing", str("0"))
		exec(`UPDATE structs.player_address SET status='approved' WHERE address=$1`, retained)
		recompute(InventoryFromDeltas(nil))
		balance("player", p2, "ualpha.defusing", str("5"))
		// Capture the entire current model, then compare every repair route.
		projectionBoundary(t, ctx, tx, 100)
		height = 100
		// The vault source is ahead of this cursor, so it is unknown again.
		recompute(InventoryFromDeltas(nil))
		value(nil)
		snapshot := func() string {
			t.Helper()
			var got string
			if err := tx.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(owner_type,owner_id,denom,balance)
ORDER BY owner_type,owner_id,denom)::text FROM structs.api_inventory WHERE owner_id=ANY($1::varchar[])`,
				[]string{p1, p2, vault, retained}).Scan(&got); err != nil {
				t.Fatal(err)
			}
			return got
		}
		want := snapshot()
		if err := backfillInventory(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if got := snapshot(); got != want {
			t.Fatalf("full rebuild changed current claims: %s != %s", got, want)
		}
		var drift int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_inventory_reconcile(FALSE)`).Scan(&drift); err != nil || drift != 0 {
			t.Fatalf("current-claim reconciliation: count=%d err=%v", drift, err)
		}
		exec(`UPDATE structs.api_inventory SET balance=0 WHERE owner_id=$1 AND denom='ualpha.infused'`, vault)
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_inventory_reconcile(TRUE)`).Scan(&drift); err != nil || drift != 1 {
			t.Fatalf("unknown-to-zero corruption not detected: count=%d err=%v", drift, err)
		}
		if n, err := consumeDriftReady(ctx, tx); err != nil || n != 1 {
			t.Fatalf("current repair: count=%d err=%v", n, err)
		}
		if got := snapshot(); got != want {
			t.Fatalf("repair restored stale receipts: %s != %s", got, want)
		}
		value(nil)
	})
}

func TestClaimAuthorityRequiresRebuildAndSurvivesInvalidation(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := refreshModels(ctx, tx, modelNames, 100, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE structs.api_refresh_state SET inventory_authority='ledger' WHERE model='inventory'`); err != nil {
			t.Fatal(err)
		}
		if needed, err := NeedsBackfill(ctx, tx, 100); err != nil || !needed {
			t.Fatalf("legacy projection must rebuild: needed=%v err=%v", needed, err)
		}
		if err := refresh(ctx, tx, "inventory", 100, stamp); err != nil {
			t.Fatal(err)
		}
		if needed, err := NeedsBackfill(ctx, tx, 100); err != nil || needed {
			t.Fatalf("current projection unexpectedly stale: needed=%v err=%v", needed, err)
		}
		if err := Invalidate(ctx, tx); err != nil {
			t.Fatal(err)
		}
		var authority string
		if err := tx.QueryRow(ctx, `SELECT inventory_authority FROM structs.api_refresh_state WHERE model='inventory'`).Scan(&authority); err != nil || authority != "current-claims-v1" {
			t.Fatalf("invalidation forgot active authority: %s %v", authority, err)
		}
		for _, height := range []int64{0, 100} {
			if needed, err := NeedsBackfill(ctx, tx, height); err != nil || !needed {
				t.Fatalf("invalidated projection at %d must rebuild: needed=%v err=%v", height, needed, err)
			}
		}
	})
}
