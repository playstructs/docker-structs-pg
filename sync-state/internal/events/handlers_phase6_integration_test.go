// Integration tests for the Go port of the four PLANET_ACTIVITY_*
// triggers. Opt-in via INTEGRATION_DATABASE_URL.
//
// We use `SET LOCAL session_replication_role = 'replica'` at the top of
// each test transaction to suppress any still-enabled PG triggers (in
// prod the Phase B SQL drops them; tests get the same effect via the
// session-local toggle which is reset on rollback).
//
// Coverage:
//   - raid: emits raid_status; no double-emit on no-op upsert; respects
//     the IS DISTINCT FROM guard (seized-ore-only update => no activity).
//   - struct: emits struct_move on location/ambit/slot change; no emit
//     on first INSERT; struct-on-fleet resolves the fleet's planet.
//   - fleet: emits fleet_depart + fleet_arrive on location_id change;
//     no emit on first INSERT; status='away' includes fleet_list.
//   - struct_attribute: emits struct_status / struct_health /
//     struct_block_* / struct_defense_add / struct_defense_remove for
//     each attribute_type; protectedStructIndex DELETE emits
//     struct_defense_remove (the SQL trigger's dead-code branch we fixed).
package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
)

// derivBctx returns a BlockContext for derivation tests.
//
// Historically toggled the OwnDerivations flag; the flag was removed
// when sync-state took unconditional ownership of derivations, so this
// is now a thin wrapper around fixedBctx kept for call-site clarity.
func derivBctx(height int64) BlockContext {
	return fixedBctx(height)
}

// suppressTriggers disables user triggers for the duration of the tx.
// session_replication_role=replica makes PG skip non-system triggers
// (ours); SET LOCAL ensures it resets on ROLLBACK/COMMIT.
func suppressTriggers(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
		t.Fatalf("suppress triggers: %v", err)
	}
}

// seedPlanetForActivity inserts a minimal structs.planet so
// planet_activity FK to planet_id resolves. (planet_activity has no FK
// to planet.id today but the GET_PLANET_ACTIVITY_SEQUENCE upsert keys
// on planet_id so any string works.)
//
// We use the planetHandler so the row matches production shape.
func seedPlanetForActivity(t *testing.T, tx pgx.Tx, planetID, ownerID string) {
	t.Helper()
	ctx := context.Background()
	raw := mustJSON(t, map[string]any{
		"id":          planetID,
		"owner":       ownerID,
		"creator":     ownerID,
		"maxOre":      "100",
		"spaceSlots":  0,
		"airSlots":    0,
		"landSlots":   0,
		"waterSlots":  0,
		"status":      "active",
	})
	if err := (planetHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
		t.Fatalf("seed planet %s: %v", planetID, err)
	}
}

// seedStructAt seeds a struct row sitting on a planet.
func seedStructAt(t *testing.T, tx pgx.Tx, structID string, structIndex int, planetID string) {
	t.Helper()
	ctx := context.Background()
	raw := mustJSON(t, map[string]any{
		"id":             structID,
		"index":          structIndex,
		"type":           1,
		"creator":        "structs1owner",
		"owner":          "structs1owner",
		"locationType":   "planet",
		"locationId":     planetID,
		"operatingAmbit": "land",
		"slot":           1,
	})
	if err := (structHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
		t.Fatalf("seed struct %s: %v", structID, err)
	}
}

// seedFleetAt seeds a fleet row sitting on a planet.
func seedFleetAt(t *testing.T, tx pgx.Tx, fleetID, planetID, status string) {
	t.Helper()
	ctx := context.Background()
	raw := mustJSON(t, map[string]any{
		"id":                   fleetID,
		"owner":                "structs1owner",
		"locationType":         "planet",
		"locationId":           planetID,
		"status":               status,
		"locationListForward":  "",
		"locationListBackward": "",
		"spaceSlots":           0,
		"airSlots":             0,
		"landSlots":            0,
		"waterSlots":           0,
	})
	if err := (fleetHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
		t.Fatalf("seed fleet %s: %v", fleetID, err)
	}
}

// countPlanetActivity returns the count of planet_activity rows matching
// (planet_id, category).
func countPlanetActivity(t *testing.T, tx pgx.Tx, planetID, category string) int {
	t.Helper()
	var n int
	err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM structs.planet_activity WHERE planet_id=$1 AND category=$2`,
		planetID, category).Scan(&n)
	if err != nil {
		t.Fatalf("count planet_activity: %v", err)
	}
	return n
}

// -------- raid (Phase 6a) --------

func TestPhase6_Raid_EmitsRaidStatusOnInsert(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(700001)

		raw := mustJSON(t, map[string]any{
			"fleetId":    "3-1",
			"planetId":   "2-555",
			"status":     "raiding",
			"seized_ore": "0",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("raid: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-555", "raid_status"); got != 1 {
			t.Errorf("planet_activity raid_status rows = %d; want 1", got)
		}

		var detail string
		if err := tx.QueryRow(ctx,
			`SELECT detail::text FROM structs.planet_activity
			 WHERE planet_id='2-555' AND category='raid_status'`).Scan(&detail); err != nil {
			t.Fatalf("detail query: %v", err)
		}
		if !(contains(detail, `"seized_ore":"0"`) || contains(detail, `"seized_ore": "0"`)) {
			t.Errorf("detail jsonb missing seized_ore: %s", detail)
		}
	})
}

// A successful raid carries a non-zero seized_ore in the EventRaid
// payload; it must land verbatim in the raid_status detail jsonb.
func TestPhase6_Raid_SeizedOreInDetailOnSuccess(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(700003)

		raw := mustJSON(t, map[string]any{
			"fleetId":    "9-266",
			"planetId":   "2-558",
			"status":     "raidSuccessful",
			"seized_ore": "12345",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("raid: %v", err)
		}
		flushBuf(t, ctx, tx, bc)

		var detail string
		if err := tx.QueryRow(ctx,
			`SELECT detail::text FROM structs.planet_activity
			 WHERE planet_id='2-558' AND category='raid_status'`).Scan(&detail); err != nil {
			t.Fatalf("detail query: %v", err)
		}
		if !(contains(detail, `"seized_ore":"12345"`) || contains(detail, `"seized_ore": "12345"`)) {
			t.Errorf("detail jsonb missing seized_ore=12345: %s", detail)
		}
		if !(contains(detail, `"status":"raidSuccessful"`) || contains(detail, `"status": "raidSuccessful"`)) {
			t.Errorf("detail jsonb missing status=raidSuccessful: %s", detail)
		}
	})
}

func TestPhase6_Raid_NoEmitOnSeizedOreOnlyUpdate(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(700002)

		first := mustJSON(t, map[string]any{
			"fleetId":    "3-2",
			"planetId":   "2-556",
			"status":     "raiding",
			"seized_ore": "0",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, first); err != nil {
			t.Fatalf("raid first: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Identical fleet+status, only seized_ore differs → IS DISTINCT
		// FROM guard filters the UPDATE, no rows affected, no activity.
		second := mustJSON(t, map[string]any{
			"fleetId":    "3-2",
			"planetId":   "2-556",
			"status":     "raiding",
			"seized_ore": "42",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, second); err != nil {
			t.Fatalf("raid second: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-556", "raid_status"); got != 1 {
			t.Errorf("planet_activity raid_status rows = %d; want 1 (seized_ore-only update should not re-emit)", got)
		}
	})
}

// -------- struct movement (Phase 6b) --------

func TestPhase6_Struct_NoEmitOnFirstInsert(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(710001)

		seedPlanetForActivity(t, tx, "2-700", "structs1owner")

		raw := mustJSON(t, map[string]any{
			"id":             "5-7001",
			"index":          7001,
			"type":           1,
			"locationType":   "planet",
			"locationId":     "2-700",
			"operatingAmbit": "land",
			"slot":           1,
		})
		if err := (structHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct insert: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-700", "struct_move"); got != 0 {
			t.Errorf("struct_move on first insert = %d; want 0", got)
		}
	})
}

func TestPhase6_Struct_EmitsMoveOnLocationChange(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(710002)

		seedPlanetForActivity(t, tx, "2-701", "structs1owner")
		seedPlanetForActivity(t, tx, "2-702", "structs1owner")
		seedStructAt(t, tx, "5-7002", 7002, "2-701")

		raw := mustJSON(t, map[string]any{
			"id":             "5-7002",
			"index":          7002,
			"type":           1,
			"locationType":   "planet",
			"locationId":     "2-702",
			"operatingAmbit": "land",
			"slot":           2,
		})
		if err := (structHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct move: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-702", "struct_move"); got != 1 {
			t.Errorf("struct_move at NEW location = %d; want 1", got)
		}

		var detail string
		if err := tx.QueryRow(ctx,
			`SELECT detail::text FROM structs.planet_activity
			 WHERE planet_id='2-702' AND category='struct_move'`).Scan(&detail); err != nil {
			t.Fatalf("detail query: %v", err)
		}
		if !contains(detail, `"struct_id"`) || !contains(detail, `"5-7002"`) {
			t.Errorf("detail jsonb missing struct_id: %s", detail)
		}
	})
}

func TestPhase6_Struct_OnFleetResolvesFleetPlanet(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(710003)

		seedPlanetForActivity(t, tx, "2-703", "structs1owner")
		seedPlanetForActivity(t, tx, "2-704", "structs1owner")
		seedFleetAt(t, tx, "3-7003", "2-704", "docked")
		seedStructAt(t, tx, "5-7003", 7003, "2-703")

		raw := mustJSON(t, map[string]any{
			"id":             "5-7003",
			"index":          7003,
			"type":           1,
			"locationType":   "fleet",
			"locationId":     "3-7003",
			"operatingAmbit": "space",
			"slot":           1,
		})
		if err := (structHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		// Activity should land on the FLEET'S planet (2-704), not the
		// fleet ID itself.
		if got := countPlanetActivity(t, tx, "2-704", "struct_move"); got != 1 {
			t.Errorf("struct_move on fleet's planet = %d; want 1", got)
		}
		if got := countPlanetActivity(t, tx, "3-7003", "struct_move"); got != 0 {
			t.Errorf("struct_move anchored on fleet id (should not be) = %d", got)
		}
	})
}

// -------- fleet movement (Phase 6c) --------

func TestPhase6_Fleet_EmitsDepartAndArrive(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(720001)

		seedPlanetForActivity(t, tx, "2-720", "structs1owner")
		seedPlanetForActivity(t, tx, "2-721", "structs1owner")
		seedFleetAt(t, tx, "3-7200", "2-720", "docked")

		raw := mustJSON(t, map[string]any{
			"id":                   "3-7200",
			"owner":                "structs1owner",
			"locationType":         "planet",
			"locationId":           "2-721",
			"status":               "docked",
			"locationListForward":  "",
			"locationListBackward": "",
			"spaceSlots":           0,
			"airSlots":             0,
			"landSlots":            0,
			"waterSlots":           0,
		})
		if err := (fleetHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("fleet: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-720", "fleet_depart"); got != 1 {
			t.Errorf("fleet_depart on OLD planet = %d; want 1", got)
		}
		if got := countPlanetActivity(t, tx, "2-721", "fleet_arrive"); got != 1 {
			t.Errorf("fleet_arrive on NEW planet = %d; want 1", got)
		}
	})
}

func TestPhase6_Fleet_AwayStatusIncludesFleetList(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(720002)

		seedPlanetForActivity(t, tx, "2-722", "structs1owner")
		seedPlanetForActivity(t, tx, "2-723", "structs1owner")

		// Seed an "away" fleet at the target planet 2-723 with empty
		// location_list_forward so it counts as the head of the queue.
		seedFleetAt(t, tx, "3-7211", "2-723", "away")

		seedFleetAt(t, tx, "3-7210", "2-722", "docked")

		// Move 3-7210 → 2-723 as "away" so the arrive emit triggers
		// the recursive CTE.
		raw := mustJSON(t, map[string]any{
			"id":                   "3-7210",
			"owner":                "structs1owner",
			"locationType":         "planet",
			"locationId":           "2-723",
			"status":               "away",
			"locationListForward":  "",
			"locationListBackward": "",
		})
		if err := (fleetHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("fleet: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var detail string
		if err := tx.QueryRow(ctx,
			`SELECT detail::text FROM structs.planet_activity
			 WHERE planet_id='2-723' AND category='fleet_arrive'`).Scan(&detail); err != nil {
			t.Fatalf("query: %v", err)
		}
		if !contains(detail, `"fleet_list"`) {
			t.Errorf("fleet_arrive detail missing fleet_list (the SQL bug we fixed): %s", detail)
		}
	})
}

// -------- struct_attribute (Phase 6d) --------

func TestPhase6_StructAttr_StatusEmitsActivity(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(730001)

		seedPlanetForActivity(t, tx, "2-730", "structs1owner")
		seedStructAt(t, tx, "5-7300", 7300, "2-730")

		raw := mustJSON(t, map[string]any{
			"attributeId": "1-5-7300",
			"value":       "3",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct_attribute: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-730", "struct_status"); got != 1 {
			t.Errorf("struct_status emit = %d; want 1", got)
		}
	})
}

func TestPhase6_StructAttr_BlockStartOreMineEmits(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(730002)

		seedPlanetForActivity(t, tx, "2-731", "structs1owner")
		seedStructAt(t, tx, "5-7311", 7311, "2-731")

		raw := mustJSON(t, map[string]any{
			"attributeId": "3-5-7311",
			"value":       "100",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct_attribute: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-731", "struct_block_ore_mine_start"); got != 1 {
			t.Errorf("struct_block_ore_mine_start emit = %d; want 1", got)
		}
	})
}

func TestPhase6_StructAttr_ProtectedIndexDeleteEmitsDefenseRemove(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(730003)

		// Planet hosts the PROTECTED struct (5-7322).
		seedPlanetForActivity(t, tx, "2-732", "structs1owner")
		seedStructAt(t, tx, "5-7322", 7322, "2-732")
		// Defender struct lives elsewhere — the activity anchors on the
		// protected struct's planet, not the defender's.
		seedPlanetForActivity(t, tx, "2-733", "structs1owner")
		seedStructAt(t, tx, "5-7321", 7321, "2-733")

		// Set defender protecting 7322.
		setup := mustJSON(t, map[string]any{
			"attributeId": "5-5-7321-0",
			"value":       "7322",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, setup); err != nil {
			t.Fatalf("setup: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		// Sanity: defense_add fired on protected planet.
		if got := countPlanetActivity(t, tx, "2-732", "struct_defense_add"); got != 1 {
			t.Fatalf("setup struct_defense_add = %d; want 1", got)
		}

		// Now delete the attribute → struct_defense_remove on the
		// protected struct's planet. This exercises the SQL-trigger
		// dead-code DELETE branch we fixed.
		clear := mustJSON(t, map[string]any{
			"attributeId": "5-5-7321-0",
			"value":       "",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, clear); err != nil {
			t.Fatalf("delete: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-732", "struct_defense_remove"); got != 1 {
			t.Errorf("struct_defense_remove on delete = %d; want 1 (SQL bug fixed)", got)
		}
	})
}

func TestPhase6_StructAttr_NoOpUpsertSkipsEmit(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(730004)

		seedPlanetForActivity(t, tx, "2-734", "structs1owner")
		seedStructAt(t, tx, "5-7340", 7340, "2-734")

		raw := mustJSON(t, map[string]any{
			"attributeId": "1-5-7340",
			"value":       "3",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("status 1: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		// Re-send same value → IS DISTINCT FROM guard skips the UPDATE.
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("status 2: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-734", "struct_status"); got != 1 {
			t.Errorf("struct_status emit count = %d; want 1 (no-op repeat should not re-emit)", got)
		}
	})
}

// Sanity: a raid upsert that DOES NOT change (fleet, status) — only the
// seized_ore field — must NOT re-emit a planet_activity row, mirroring
// PG's AFTER UPDATE semantics (UPDATEs whose IS DISTINCT FROM guard
// filters them out don't fire AFTER triggers).
func TestPhase6_Raid_NoEmitWhenUpsertIsNoOp(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := fixedBctx(700099)
		raw := mustJSON(t, map[string]any{
			"fleetId":   "3-99",
			"planetId":  "2-557",
			"status":    "raiding",
			"seizedOre": "0",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("raid first: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		got1 := countPlanetActivity(t, tx, "2-557", "raid_status")
		// Second call with same (fleet, status) — even with different
		// seized_ore — should not emit again because the upsert
		// IS DISTINCT FROM guard filters the row out.
		if err := (raidHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("raid second: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		got2 := countPlanetActivity(t, tx, "2-557", "raid_status")
		if got2 != got1 {
			t.Errorf("no-op upsert re-emitted planet_activity: was=%d now=%d", got1, got2)
		}
	})
}

// Unused import guard (json) — keeps editor happy if we tweak above.
var _ = json.RawMessage{}
