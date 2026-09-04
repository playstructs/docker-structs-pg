// Integration tests for Phase 4 handlers (grid / struct_attribute /
// planet_attribute). Same opt-in pattern as the other phases: requires
// INTEGRATION_DATABASE_URL and rolls back its transaction at the end of
// every test so the dev DB stays clean.
package events

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
)

// -------- grid --------

func TestHandler_Grid_OreUpsert_WritesStat(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"attributeId": "0-5-4242", // gridAttrType=ore, struct, index 4242
			"value":       "1234",
		})
		handle(t, ctx, tx, gridHandler{}, bctx(), raw)
		var atype, otype, oid string
		var oidx int
		var val int64
		_ = tx.QueryRow(ctx,
			`SELECT attribute_type, object_type, object_index, object_id, val
			 FROM structs.grid WHERE id=$1`,
			"0-5-4242").Scan(&atype, &otype, &oidx, &oid, &val)
		if atype != "ore" || otype != "struct" || oidx != 4242 || oid != "5-4242" || val != 1234 {
			t.Errorf("grid row: atype=%q otype=%q oidx=%d oid=%q val=%d", atype, otype, oidx, oid, val)
		}

		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_ore
			 WHERE object_type='struct' AND object_index=$1 AND value=$2`,
			4242, 1234).Scan(&n)
		if n != 1 {
			t.Errorf("expected 1 stat_ore row, got %d", n)
		}
	})
}

func TestHandler_Grid_StructsLoad_NoObjectType(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// sub-index 4 = structsLoad — table-stat.sql:99-103 has no
		// object_type column. Verify we hit the right INSERT.
		raw := mustJSON(t, map[string]any{
			"attributeId": "4-5-1717",
			"value":       "9",
		})
		handle(t, ctx, tx, gridHandler{}, bctx(), raw)
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_structs_load WHERE object_index=$1 AND value=$2`,
			1717, 9).Scan(&n)
		if n != 1 {
			t.Errorf("expected 1 stat_structs_load row, got %d", n)
		}
	})
}

func TestHandler_Grid_HighSubIndex_NoStat(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// sub-index 10 = proxyNonce — grid-only, no stat partner.
		raw := mustJSON(t, map[string]any{
			"attributeId": "10-5-7777",
			"value":       "42",
		})
		handle(t, ctx, tx, gridHandler{}, bctx(), raw)
		var atype string
		_ = tx.QueryRow(ctx, `SELECT attribute_type FROM structs.grid WHERE id=$1`, "10-5-7777").Scan(&atype)
		if atype != "proxyNonce" {
			t.Errorf("atype = %q want proxyNonce", atype)
		}
	})
}

func TestHandler_Grid_DeleteWritesZeroSentinel(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// First insert so there's a row to delete.
		ins := mustJSON(t, map[string]any{"attributeId": "1-5-808", "value": "55"})
		handle(t, ctx, tx, gridHandler{}, bctx(), ins)
		// Now delete with value="" — the SQL inserts a value=0 sentinel
		// into stat_fuel because gridAttrType=1.
		del := mustJSON(t, map[string]any{"attributeId": "1-5-808", "value": ""})
		handle(t, ctx, tx, gridHandler{}, bctx(), del)
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.grid WHERE id=$1`, "1-5-808").Scan(&n)
		if n != 0 {
			t.Errorf("expected grid row deleted, count=%d", n)
		}
		var zeros int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_fuel
			 WHERE object_type='struct' AND object_index=$1 AND value=0`,
			808).Scan(&zeros)
		if zeros != 1 {
			t.Errorf("expected 1 zero sentinel in stat_fuel, got %d", zeros)
		}
	})
}

func TestHandler_Grid_NoOpUpsertSkipsStat(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Insert once, then upsert with the same val — the IS DISTINCT
		// FROM guard means the second upsert is a no-op and the stat
		// hypertable should NOT get a duplicate sample.
		raw := mustJSON(t, map[string]any{"attributeId": "5-5-9090", "value": "777"})
		handle(t, ctx, tx, gridHandler{}, bctx(), raw)
		handle(t, ctx, tx, gridHandler{}, bctx(), raw)
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_power
			 WHERE object_type='struct' AND object_index=$1`,
			9090).Scan(&n)
		if n != 1 {
			t.Errorf("expected 1 stat_power row across two identical upserts, got %d", n)
		}
	})
}

// -------- struct_attribute --------

func TestHandler_StructAttribute_HealthUpsert_WritesStat(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"attributeId": "0-5-1234", // health for struct index 1234
			"value":       "97",
		})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), raw)
		var atype, otype, oid string
		var subIdx int
		var val int64
		_ = tx.QueryRow(ctx,
			`SELECT attribute_type, object_type, object_id, sub_index, val
			 FROM structs.struct_attribute WHERE id=$1`,
			"0-5-1234").Scan(&atype, &otype, &oid, &subIdx, &val)
		if atype != "health" || otype != "struct" || oid != "5-1234" || subIdx != 0 || val != 97 {
			t.Errorf("row: atype=%q otype=%q oid=%q subIdx=%d val=%d", atype, otype, oid, subIdx, val)
		}
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_struct_health WHERE object_index=$1 AND value=$2`,
			1234, 97).Scan(&n)
		if n != 1 {
			t.Errorf("expected 1 stat_struct_health row, got %d", n)
		}
	})
}

func TestHandler_StructAttribute_ZeroValueKeepsRow(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		seed := mustJSON(t, map[string]any{"attributeId": "0-5-9999", "value": "50"})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), seed)
		zero := mustJSON(t, map[string]any{"attributeId": "0-5-9999", "value": "0"})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), zero)
		var atype string
		var val int64
		if err := tx.QueryRow(ctx,
			`SELECT attribute_type, val FROM structs.struct_attribute WHERE id=$1`,
			"0-5-9999").Scan(&atype, &val); err != nil {
			t.Fatalf("row after value=0: %v", err)
		}
		if atype != "health" || val != 0 {
			t.Errorf("after value=0: atype=%q val=%d want health/0", atype, val)
		}
		var zeros int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_struct_health WHERE object_index=$1 AND value=0`,
			9999).Scan(&zeros)
		if zeros != 1 {
			t.Errorf("expected 1 zero sentinel in stat_struct_health on clear, got %d", zeros)
		}
	})
}

// TestHandler_StructAttribute_StatusClearPreservesRow ensures empty/"0"
// status does not wipe the destroyed bit — /api/struct/player/{id} uses
// COALESCE(sa_status.val, 0) during STRUCT_SWEEP_DELAY.
func TestHandler_StructAttribute_StatusClearPreservesRow(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		seed := mustJSON(t, map[string]any{"attributeId": "1-5-9998", "value": "32"})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), seed)

		for _, clear := range []string{"0", ""} {
			handle(t, ctx, tx, structAttributeHandler{}, bctx(),
				mustJSON(t, map[string]any{"attributeId": "1-5-9998", "value": clear}))
			var val int64
			if err := tx.QueryRow(ctx,
				`SELECT val FROM structs.struct_attribute WHERE id=$1`,
				"1-5-9998").Scan(&val); err != nil {
				t.Fatalf("status after clear %q: %v", clear, err)
			}
			if val != 32 {
				t.Errorf("status after clear %q: val=%d want 32 (preserved)", clear, val)
			}
		}
	})
}

func TestHandler_StructAttribute_StatusBit32_StampsDestroy(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Seed the struct row (via the Phase 2 handler so PG triggers
		// fire as in production) — destroyed_block UPDATE targets it.
		structRaw := mustJSON(t, map[string]any{
			"id":             "5-3030",
			"index":          3030,
			"type":           1,
			"creator":        "creator",
			"owner":          "1-1",
			"locationType":   "planet",
			"locationId":     "2-1",
			"operatingAmbit": "LAND",
			"slot":           1,
		})
		handle(t, ctx, tx, structHandler{}, bctx(), structRaw)

		// Status with bit 32 set (value=32 is the cleanest "just destroyed").
		statusRaw := mustJSON(t, map[string]any{
			"attributeId": "1-5-3030", // status for struct 3030
			"value":       "32",
		})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), statusRaw)

		var destroyed bool
		var dblock *int64
		_ = tx.QueryRow(ctx,
			`SELECT is_destroyed, destroyed_block FROM structs.struct WHERE id=$1`,
			"5-3030").Scan(&destroyed, &dblock)
		if !destroyed {
			t.Errorf("is_destroyed = false; want true")
		}
		if dblock == nil || *dblock != bctx().Height {
			t.Errorf("destroyed_block = %v; want %d", dblock, bctx().Height)
		}
		var statCount int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_struct_status WHERE object_index=$1 AND value=32`,
			3030).Scan(&statCount)
		if statCount != 1 {
			t.Errorf("expected 1 stat_struct_status row, got %d", statCount)
		}
	})
}

// TestHandler_StructAttribute_StatusBit32_ClearsOrphanedDefenders verifies
// the DB-side fix for the chain's laziness about cleaning defender
// relationships when the *protected* struct is destroyed: both
// struct_defender rows, both paired protectedStructIndex attributes, and
// one struct_defense_remove planet_activity row per defender.
func TestHandler_StructAttribute_StatusBit32_ClearsOrphanedDefenders(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(900100)

		const (
			planetID    = testDestroyPlanet
			protectedID = testDestroyProtected
			defenderA   = testDestroyDefenderA
			defenderB   = testDestroyDefenderB
		)

		seedPlanetForActivity(t, tx, planetID, "structs1owner")
		seedStructAt(t, tx, protectedID, testDestroyProtectedIndex, planetID)

		for _, def := range []string{defenderA, defenderB} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO structs.struct_defender (defending_struct_id, protected_struct_id, updated_at)
				 VALUES ($1, $2, NOW())`, def, protectedID); err != nil {
				t.Fatalf("seed defender %s: %v", def, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO structs.struct_attribute (id, object_id, object_type, sub_index, attribute_type, val, updated_at)
				 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
				"5-"+def, def, "struct", 0, "protectedStructIndex", testDestroyProtectedIndex); err != nil {
				t.Fatalf("seed protectedStructIndex for %s: %v", def, err)
			}
		}

		// attributeId is "{attrType}-{objTypeId}-{objIndex}": status (1)
		// on the struct object type (5), value 32 = the destroyed bit.
		statusRaw := mustJSON(t, map[string]any{
			"attributeId": "1-5-" + strconv.Itoa(testDestroyProtectedIndex),
			"value":       "32",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, statusRaw); err != nil {
			t.Fatalf("destroy status: %v", err)
		}
		flushBuf(t, ctx, tx, bc)

		var defenderCount int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.struct_defender WHERE protected_struct_id=$1`,
			protectedID).Scan(&defenderCount)
		if defenderCount != 0 {
			t.Errorf("struct_defender rows remaining for protected=%s: %d want 0", protectedID, defenderCount)
		}

		var attrCount int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.struct_attribute
			  WHERE id IN ($1, $2)`,
			"5-"+defenderA, "5-"+defenderB).Scan(&attrCount)
		if attrCount != 0 {
			t.Errorf("protectedStructIndex attrs remaining: %d want 0", attrCount)
		}

		rows, err := tx.Query(ctx,
			`SELECT detail->>'defender_struct_id', detail->>'protected_struct_id', planet_id
			   FROM structs.planet_activity
			  WHERE category='struct_defense_remove'
			    AND detail->>'protected_struct_id'=$1
			  ORDER BY detail->>'defender_struct_id'`,
			protectedID)
		if err != nil {
			t.Fatalf("query defense_remove: %v", err)
		}
		defer rows.Close()

		got := make(map[string]string) // defender -> planet
		for rows.Next() {
			var defID, protID, pid string
			if err := rows.Scan(&defID, &protID, &pid); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if protID != protectedID {
				t.Errorf("protected_struct_id = %q want %q", protID, protectedID)
			}
			got[defID] = pid
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("struct_defense_remove rows = %d want 2; got=%v", len(got), got)
		}
		for _, def := range []string{defenderA, defenderB} {
			if pid, ok := got[def]; !ok {
				t.Errorf("missing defense_remove for defender %s", def)
			} else if pid != planetID {
				t.Errorf("defense_remove planet for %s = %q want %q", def, pid, planetID)
			}
		}
	})
}

func TestHandler_StructAttribute_StatusNoBit32_LeavesStructAlone(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Seed struct in known state.
		structRaw := mustJSON(t, map[string]any{
			"id":             "5-4040",
			"index":          4040,
			"type":           1,
			"creator":        "creator",
			"owner":          "1-1",
			"locationType":   "planet",
			"locationId":     "2-1",
			"operatingAmbit": "LAND",
			"slot":           1,
		})
		handle(t, ctx, tx, structHandler{}, bctx(), structRaw)

		// Seed a defender relationship that must survive a non-destroy status.
		if _, err := tx.Exec(ctx,
			`INSERT INTO structs.struct_defender (defending_struct_id, protected_struct_id, updated_at)
			 VALUES ($1, $2, NOW())`, testSurvivingDefender, "5-4040"); err != nil {
			t.Fatalf("seed defender: %v", err)
		}

		// Status without bit 32 (value=1, e.g., online flag).
		statusRaw := mustJSON(t, map[string]any{
			"attributeId": "1-5-4040",
			"value":       "1",
		})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), statusRaw)

		var destroyed bool
		var dblock *int64
		_ = tx.QueryRow(ctx,
			`SELECT is_destroyed, destroyed_block FROM structs.struct WHERE id=$1`,
			"5-4040").Scan(&destroyed, &dblock)
		if destroyed {
			t.Errorf("is_destroyed = true; want false")
		}
		if dblock != nil {
			t.Errorf("destroyed_block = %v; want NULL", *dblock)
		}

		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.struct_defender WHERE defending_struct_id=$1 AND protected_struct_id=$2`,
			testSurvivingDefender, "5-4040").Scan(&n)
		if n != 1 {
			t.Errorf("defender row count = %d; want 1 (non-destroy status must leave it alone)", n)
		}
	})
}

func TestHandler_StructAttribute_SubIndexParsed(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Use attrType=6 (typeCount) with explicit sub_index=2 to verify
		// the 4th split-part lands in struct_attribute.sub_index.
		raw := mustJSON(t, map[string]any{
			"attributeId": "6-5-501-2",
			"value":       "13",
		})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), raw)
		var subIdx int
		var atype string
		_ = tx.QueryRow(ctx,
			`SELECT sub_index, attribute_type FROM structs.struct_attribute WHERE id=$1`,
			"6-5-501-2").Scan(&subIdx, &atype)
		if subIdx != 2 || atype != "typeCount" {
			t.Errorf("subIdx=%d atype=%q want 2/typeCount", subIdx, atype)
		}
	})
}

// -------- planet_attribute --------

func TestHandler_PlanetAttribute_UpsertAndClear(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Insert: planetaryShield (attrType=0) on planet index 7.
		ins := mustJSON(t, map[string]any{
			"attributeId": "0-2-7",
			"value":       "5",
		})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), ins)
		var atype, otype, oid string
		var val int64
		_ = tx.QueryRow(ctx,
			`SELECT attribute_type, object_type, object_id, val
			 FROM structs.planet_attribute WHERE id=$1`,
			"0-2-7").Scan(&atype, &otype, &oid, &val)
		if atype != "planetaryShield" || otype != "planet" || oid != "2-7" || val != 5 {
			t.Errorf("row: atype=%q otype=%q oid=%q val=%d", atype, otype, oid, val)
		}

		// Update: same id, different val.
		upd := mustJSON(t, map[string]any{"attributeId": "0-2-7", "value": "9"})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), upd)
		_ = tx.QueryRow(ctx, `SELECT val FROM structs.planet_attribute WHERE id=$1`, "0-2-7").Scan(&val)
		if val != 9 {
			t.Errorf("after update val=%d want 9", val)
		}

		// Clear via value="0" — keep-zero leaves the row at val=0.
		zero := mustJSON(t, map[string]any{"attributeId": "0-2-7", "value": "0"})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), zero)
		_ = tx.QueryRow(ctx,
			`SELECT attribute_type, val FROM structs.planet_attribute WHERE id=$1`,
			"0-2-7").Scan(&atype, &val)
		if atype != "planetaryShield" || val != 0 {
			t.Errorf("after value=0: atype=%q val=%d want planetaryShield/0", atype, val)
		}
	})
}

func TestHandler_PlanetAttribute_EmptyValueClears(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ins := mustJSON(t, map[string]any{"attributeId": "10-2-50", "value": "1"})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), ins)
		del := mustJSON(t, map[string]any{"attributeId": "10-2-50", "value": ""})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), del)
		var atype string
		var val int64
		if err := tx.QueryRow(ctx,
			`SELECT attribute_type, val FROM structs.planet_attribute WHERE id=$1`,
			"10-2-50").Scan(&atype, &val); err != nil {
			t.Fatalf("row after empty value: %v", err)
		}
		if atype != "blockStartRaid" || val != 0 {
			t.Errorf("after empty: atype=%q val=%d want blockStartRaid/0", atype, val)
		}
	})
}

func TestHandler_PlanetAttribute_AllLabels(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Spot-check every attrType label (0..15) maps to the correct
		// attribute_type column value. Use planet index 8888 so cleanup
		// (transaction rollback) doesn't interfere with anything real.
		for attrType, want := range planetAttrLabels {
			id := mustJSON(t, map[string]any{
				"attributeId": ggToID(attrType, 2, 8888),
				"value":       "1",
			})
			handle(t, ctx, tx, planetAttributeHandler{}, bctx(), id)
			var got string
			_ = tx.QueryRow(ctx,
				`SELECT attribute_type FROM structs.planet_attribute WHERE id=$1`,
				ggToID(attrType, 2, 8888)).Scan(&got)
			if got != want {
				t.Errorf("attrType=%d: got %q want %q", attrType, got, want)
			}
		}
	})
}

// TestHandler_PlanetAttribute_ShieldChangeActivity verifies that v0.18.0
// planetaryShield (attrType 0) changes emit a shield_change planet_activity
// row carrying old + new values, including the change down to zero.
func TestHandler_PlanetAttribute_ShieldChangeActivity(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// 0 -> 5, 5 -> 9, 9 -> 0 on a reserved-range planet. The first
		// transition's old value is only 0 for a planet with no existing
		// planetaryShield attribute, so a live planet index would report
		// its real shield as the old value.
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "0-2-999050", "value": "5"}))
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "0-2-999050", "value": "9"}))
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "0-2-999050", "value": "0"}))

		type pair struct{ newV, oldV int64 }
		want := []pair{{5, 0}, {9, 5}, {0, 9}}

		rows, err := tx.Query(ctx,
			`SELECT (detail->>'planetary_shield')::bigint,
			        (detail->>'planetary_shield_old')::bigint
			   FROM structs.planet_activity
			  WHERE planet_id='2-999050' AND category='shield_change'
			  ORDER BY seq`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.newV, &p.oldV); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, p)
		}
		if len(got) != len(want) {
			t.Fatalf("shield_change rows=%d want %d (%+v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %+v want %+v", i, got[i], want[i])
			}
		}
	})
}

// TestHandler_PlanetAttribute_BlockRaidStartActivity verifies that v0.18.0
// blockStartRaid (attrType 10) changes emit a block_raid_start planet_activity
// row carrying old + new values on both set and clear.
func TestHandler_PlanetAttribute_BlockRaidStartActivity(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// 0 -> 1234 (raid window opens), 1234 -> 0 (cleared) on planet index 50.
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "10-2-50", "value": "1234"}))
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "10-2-50", "value": ""}))

		type pair struct{ newV, oldV int64 }
		want := []pair{{1234, 0}, {0, 1234}}

		rows, err := tx.Query(ctx,
			`SELECT (detail->>'block_start_raid')::bigint,
			        (detail->>'block_start_raid_old')::bigint
			   FROM structs.planet_activity
			  WHERE planet_id='2-50' AND category='block_raid_start'
			  ORDER BY seq`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.newV, &p.oldV); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, p)
		}
		if len(got) != len(want) {
			t.Fatalf("block_raid_start rows=%d want %d (%+v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %+v want %+v", i, got[i], want[i])
			}
		}
	})
}

// TestHandler_PlanetAttribute_NoActivityForOtherAttrs verifies that
// attribute types other than planetaryShield / blockStartRaid / ore
// clocks (12/13) never emit a planet_activity row. Types 11 / 14 / 15
// are indexed only.
func TestHandler_PlanetAttribute_NoActivityForOtherAttrs(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		quiet := []struct {
			id  string
			msg string
		}{
			{"1-2-77", "attrType 1 repairNetworkQuantity"},
			{"11-2-77", "attrType 11 blockRaiderArrived"},
			{"14-2-77", "attrType 14 oreMiningActiveQuantity"},
			{"15-2-77", "attrType 15 oreRefiningActiveQuantity"},
		}
		for _, c := range quiet {
			handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
				mustJSON(t, map[string]any{"attributeId": c.id, "value": "3"}))
		}
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.planet_activity WHERE planet_id='2-77'`).Scan(&n)
		if n != 0 {
			t.Errorf("expected no planet_activity rows for quiet attrs, got %d", n)
		}
	})
}

// ggToID is a tiny helper to build attribute ids in tests without sprintf-ing.
func ggToID(attr, otype, idx int) string {
	return itoa(attr) + "-" + itoa(otype) + "-" + itoa(idx)
}

func itoa(i int) string {
	// avoid pulling strconv into a test helper used only for fixture ids
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
