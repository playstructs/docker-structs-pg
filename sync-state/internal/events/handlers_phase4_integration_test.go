// Integration tests for Phase 4 handlers (grid / struct_attribute /
// planet_attribute). Same opt-in pattern as the other phases: requires
// INTEGRATION_DATABASE_URL and rolls back its transaction at the end of
// every test so the dev DB stays clean.
package events

import (
	"context"
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

func TestHandler_StructAttribute_ZeroValueDeletes(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Seed a health row first so there's something to delete.
		seed := mustJSON(t, map[string]any{"attributeId": "0-5-9999", "value": "50"})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), seed)
		// value="0" should DELETE per the 20260203 migration.
		zero := mustJSON(t, map[string]any{"attributeId": "0-5-9999", "value": "0"})
		handle(t, ctx, tx, structAttributeHandler{}, bctx(), zero)
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.struct_attribute WHERE id=$1`, "0-5-9999").Scan(&n)
		if n != 0 {
			t.Errorf("expected row deleted on value=0, count=%d", n)
		}
		var zeros int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.stat_struct_health WHERE object_index=$1 AND value=0`,
			9999).Scan(&zeros)
		if zeros != 1 {
			t.Errorf("expected 1 zero sentinel in stat_struct_health on delete, got %d", zeros)
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

func TestHandler_PlanetAttribute_UpsertAndDelete(t *testing.T) {
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

		// Delete via value="0".
		zero := mustJSON(t, map[string]any{"attributeId": "0-2-7", "value": "0"})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), zero)
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.planet_attribute WHERE id=$1`, "0-2-7").Scan(&n)
		if n != 0 {
			t.Errorf("expected row deleted on value=0, count=%d", n)
		}
	})
}

func TestHandler_PlanetAttribute_EmptyValueDeletes(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ins := mustJSON(t, map[string]any{"attributeId": "10-2-50", "value": "1"})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), ins)
		del := mustJSON(t, map[string]any{"attributeId": "10-2-50", "value": ""})
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(), del)
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.planet_attribute WHERE id=$1`, "10-2-50").Scan(&n)
		if n != 0 {
			t.Errorf("expected row deleted on empty value, count=%d", n)
		}
	})
}

func TestHandler_PlanetAttribute_AllLabels(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Spot-check every attrType label (0..10) maps to the correct
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
