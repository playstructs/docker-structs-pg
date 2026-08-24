// Integration tests for structsd v0.21.0 planet ore clocks.
// Opt-in via INTEGRATION_DATABASE_URL.
package events

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	oreClockPlanetID    = "2-999060"
	oreClockPlanetIdx   = 999060
	oreClockStructID    = "5-999060"
	oreClockStructIdx   = 999060
	oreClockTypeID      = 999060
	oreClockPlanetVal   = int64(4242)
	oreClockLeftoverVal = int64(99)
)

// TestHandler_PlanetAttribute_OreClockTypes11to15 indexes planet attrs
// 11–15 and asserts id / object_id / attribute_type string keys / val.
func TestHandler_PlanetAttribute_OreClockTypes11to15(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		want := []struct {
			attrType int
			label    string
			val      int64
		}{
			{11, "blockRaiderArrived", 10},
			{12, "blockStartOreMine", 20},
			{13, "blockStartOreRefine", 30},
			{14, "oreMiningActiveQuantity", 2},
			{15, "oreRefiningActiveQuantity", 3},
		}
		for _, c := range want {
			id := ggToID(c.attrType, 2, oreClockPlanetIdx)
			handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
				mustJSON(t, map[string]any{"attributeId": id, "value": itoa(int(c.val))}))
			var atype, oid string
			var val int64
			if err := tx.QueryRow(ctx,
				`SELECT attribute_type, object_id, val FROM structs.planet_attribute WHERE id=$1`,
				id).Scan(&atype, &oid, &val); err != nil {
				t.Fatalf("attrType %d: %v", c.attrType, err)
			}
			if atype != c.label || oid != oreClockPlanetID || val != c.val {
				t.Errorf("attrType %d: atype=%q oid=%q val=%d want %q %q %d",
					c.attrType, atype, oid, val, c.label, oreClockPlanetID, c.val)
			}
		}
	})
}

// TestHandler_PlanetAttribute_OreClockZeroDeletes removes the planet row
// when type 12/13 is written as 0 or empty.
func TestHandler_PlanetAttribute_OreClockZeroDeletes(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		cases := []struct {
			id    string
			clear string
		}{
			{"12-2-999061", "0"},
			{"13-2-999062", ""},
		}
		for _, c := range cases {
			handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
				mustJSON(t, map[string]any{"attributeId": c.id, "value": "100"}))
			handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
				mustJSON(t, map[string]any{"attributeId": c.id, "value": c.clear}))
			var n int
			_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.planet_attribute WHERE id=$1`, c.id).Scan(&n)
			if n != 0 {
				t.Errorf("%s value=%q: expected delete, count=%d", c.id, c.clear, n)
			}
		}
	})
}

// TestHandler_PlanetAttribute_OreClockActivity emits the reused grass
// categories from planet attrs 12/13 with detail {planet_id, block},
// including a clear to zero.
func TestHandler_PlanetAttribute_OreClockActivity(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "12-2-999063", "value": "100"}))
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "12-2-999063", "value": "0"}))
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "13-2-999063", "value": "200"}))

		type row struct {
			category, planetID string
			block              int64
		}
		rows, err := tx.Query(ctx,
			`SELECT category, detail->>'planet_id', (detail->>'block')::bigint
			   FROM structs.planet_activity
			  WHERE planet_id='2-999063'
			    AND category IN ('struct_block_ore_mine_start','struct_block_ore_refine_start')
			  ORDER BY seq`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.category, &r.planetID, &r.block); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{
			{"struct_block_ore_mine_start", "2-999063", 100},
			{"struct_block_ore_mine_start", "2-999063", 0},
			{"struct_block_ore_refine_start", "2-999063", 200},
		}
		if len(got) != len(want) {
			t.Fatalf("ore-clock activity rows=%d want %d (%+v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %+v want %+v", i, got[i], want[i])
			}
		}
	})
}

func TestHandler_PlanetAttribute_ViewWorkReadsPlanetOreClock(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		requirePlanetOreClockViews(t, tx)
		suppressTriggers(t, tx)

		seedOreClockMineFixture(t, tx)

		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{
				"attributeId": "12-2-999060",
				"value":       itoa(int(oreClockPlanetVal)),
			}))
		// Leftover per-struct clock with a different value — must not win.
		if err := (structAttributeHandler{}).Handle(ctx, tx, bctx(),
			mustJSON(t, map[string]any{
				"attributeId": "3-5-999060",
				"value":       itoa(int(oreClockLeftoverVal)),
			})); err != nil {
			t.Fatalf("leftover struct attr 3: %v", err)
		}

		var blockStart int64
		if err := tx.QueryRow(ctx,
			`SELECT block_start FROM view.work
			  WHERE object_id=$1 AND category='MINE'`,
			oreClockStructID).Scan(&blockStart); err != nil {
			t.Fatalf("view.work MINE: %v", err)
		}
		if blockStart != oreClockPlanetVal {
			t.Errorf("view.work MINE block_start=%d want planet clock %d (not leftover %d)",
				blockStart, oreClockPlanetVal, oreClockLeftoverVal)
		}

		var structClock int64
		if err := tx.QueryRow(ctx,
			`SELECT block_start_ore_mine FROM view.struct WHERE struct_id=$1`,
			oreClockStructID).Scan(&structClock); err != nil {
			t.Fatalf("view.struct: %v", err)
		}
		if structClock != oreClockPlanetVal {
			t.Errorf("view.struct.block_start_ore_mine=%d want %d", structClock, oreClockPlanetVal)
		}
	})
}

func TestHandler_StructAttribute_OreClockDeleteDoesNotResurrect(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		requirePlanetOreClockViews(t, tx)
		suppressTriggers(t, tx)

		seedOreClockMineFixture(t, tx)
		handle(t, ctx, tx, planetAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{
				"attributeId": "12-2-999060",
				"value":       itoa(int(oreClockPlanetVal)),
			}))
		handle(t, ctx, tx, structAttributeHandler{}, bctx(),
			mustJSON(t, map[string]any{"attributeId": "3-5-999060", "value": "99"}))

		// Upgrade-style delete of leftover struct clocks.
		for _, id := range []string{"3-5-999060", "4-5-999060"} {
			handle(t, ctx, tx, structAttributeHandler{}, bctx(),
				mustJSON(t, map[string]any{"attributeId": id, "value": "0"}))
		}

		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.struct_attribute WHERE id IN ('3-5-999060','4-5-999060')`).Scan(&n)
		if n != 0 {
			t.Errorf("leftover struct attrs still present, count=%d", n)
		}

		var blockStart int64
		if err := tx.QueryRow(ctx,
			`SELECT block_start FROM view.work
			  WHERE object_id=$1 AND category='MINE'`,
			oreClockStructID).Scan(&blockStart); err != nil {
			t.Fatalf("view.work MINE: %v", err)
		}
		if blockStart != oreClockPlanetVal {
			t.Errorf("view.work MINE block_start=%d want planet clock %d after leftover delete",
				blockStart, oreClockPlanetVal)
		}
		if got := countPlanetActivity(t, tx, oreClockPlanetID, "struct_block_ore_mine_start"); got != 1 {
			t.Errorf("planet ore-clock activity = %d; want 1 (leftover struct writes must not emit)", got)
		}
	})
}

func requirePlanetOreClockViews(t *testing.T, tx pgx.Tx) {
	t.Helper()
	var def string
	err := tx.QueryRow(context.Background(),
		`SELECT pg_get_viewdef('view.work'::regclass)`).Scan(&def)
	if err != nil {
		t.Skipf("view.work missing: %v", err)
	}
	if !strings.Contains(def, "'12-'") {
		t.Skip("view.work has not applied view-work-20260824-planet-ore-clocks (no '12-' planet clock lookup)")
	}
}

func seedOreClockMineFixture(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	seedPlanetForActivity(t, tx, oreClockPlanetID, "structs1owner")

	rawType := mustJSON(t, map[string]any{
		"id":                                    oreClockTypeID,
		"type":                                  "TestOreRig",
		"category":                              "TEST",
		"buildLimit":                            10,
		"buildDifficulty":                       100,
		"buildDraw":                             500,
		"maxHealth":                             1000,
		"passiveDraw":                           50,
		"possibleAmbit":                         15,
		"movable":                               true,
		"slotBound":                             false,
		"primaryWeapon":                         "BLASTER",
		"primaryWeaponControl":                  "MANUAL",
		"primaryWeaponCharge":                   0,
		"primaryWeaponAmbits":                   15,
		"primaryWeaponTargets":                  1,
		"primaryWeaponShots":                    1,
		"primaryWeaponDamage":                   10,
		"primaryWeaponBlockable":                true,
		"primaryWeaponCounterable":              true,
		"primaryWeaponArmourPiercing":           false,
		"primaryWeaponRecoilDamage":             0,
		"primaryWeaponShotSuccessRateNumerator": 1,
		"primaryWeaponShotSuccessRateDenominator": 1,
		"planetaryMining":                         "oreMiningRig",
		"oreMiningDifficulty":                     14000,
		"class":                                   "Test",
	})
	if err := (structTypeHandler{}).Handle(ctx, tx, bctx(), rawType); err != nil {
		t.Fatalf("seed struct_type: %v", err)
	}

	rawStruct := mustJSON(t, map[string]any{
		"id":             oreClockStructID,
		"index":          oreClockStructIdx,
		"type":           oreClockTypeID,
		"creator":        "structs1owner",
		"owner":          "structs1owner",
		"locationType":   "planet",
		"locationId":     oreClockPlanetID,
		"operatingAmbit": "land",
		"slot":           1,
	})
	if err := (structHandler{}).Handle(ctx, tx, bctx(), rawStruct); err != nil {
		t.Fatalf("seed struct: %v", err)
	}

	// Online status bit 4.
	if err := (structAttributeHandler{}).Handle(ctx, tx, bctx(),
		mustJSON(t, map[string]any{"attributeId": "1-5-999060", "value": "4"})); err != nil {
		t.Fatalf("status: %v", err)
	}

	// Buried ore on the planet: grid id '0-' || location_id.
	handle(t, ctx, tx, gridHandler{}, bctx(),
		mustJSON(t, map[string]any{"attributeId": "0-2-999060", "value": "5"}))
}
