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
//     struct_block_build_start / struct_defense_add / struct_defense_remove
//     for each attribute_type; protectedStructIndex DELETE emits
//     struct_defense_remove (the SQL trigger's dead-code branch we fixed).
//     v0.21.0: leftover struct attrs 3/4 no longer emit ore-clock grass.
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
		"id":         planetID,
		"owner":      ownerID,
		"creator":    ownerID,
		"maxOre":     "100",
		"spaceSlots": 0,
		"airSlots":   0,
		"landSlots":  0,
		"waterSlots": 0,
		"status":     "active",
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

		// Reserved-range ids so this really is a first insert: a struct the
		// chain already placed elsewhere would arrive as a location change
		// and legitimately emit struct_move.
		seedPlanetForActivity(t, tx, "2-999020", "structs1owner")

		raw := mustJSON(t, map[string]any{
			"id":             "5-999020",
			"index":          999020,
			"type":           1,
			"locationType":   "planet",
			"locationId":     "2-999020",
			"operatingAmbit": "land",
			"slot":           1,
		})
		if err := (structHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct insert: %v", err)
		}
		flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-999020", "struct_move"); got != 0 {
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

		seedPlanetForActivity(t, tx, "2-999030", "structs1owner")
		seedPlanetForActivity(t, tx, "2-999031", "structs1owner")
		seedFleetAt(t, tx, "3-999030", "2-999030", "docked")

		raw := mustJSON(t, map[string]any{
			"id":                   "3-999030",
			"owner":                "structs1owner",
			"locationType":         "planet",
			"locationId":           "2-999031",
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
		if got := countPlanetActivity(t, tx, "2-999030", "fleet_depart"); got != 1 {
			t.Errorf("fleet_depart on OLD planet = %d; want 1", got)
		}
		if got := countPlanetActivity(t, tx, "2-999031", "fleet_arrive"); got != 1 {
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

// -------- fleet-carried planetary defenses --------

// Ids for the defense-presence tests. Reserved 990000+ range — see the
// note in handlers_integration_test.go.
const (
	testFleetDefHome           = "2-990970"
	testFleetDefEnemyA         = "2-990971"
	testFleetDefEnemyB         = "2-990972"
	testFleetDefFleet          = "3-990970"
	testFleetDefDefender       = "5-990970"
	testFleetDefDefenderIndex  = 990970
	testFleetDefProtected      = "5-990971"
	testFleetDefProtectedIndex = 990971
)

// seedStructOnFleet seeds a struct riding a fleet rather than sitting on
// a planet, so its location_type is 'fleet'.
func seedStructOnFleet(t *testing.T, tx pgx.Tx, structID string, structIndex int, fleetID string) {
	t.Helper()
	ctx := context.Background()
	raw := mustJSON(t, map[string]any{
		"id":             structID,
		"index":          structIndex,
		"type":           1,
		"creator":        "structs1owner",
		"owner":          "structs1owner",
		"locationType":   "fleet",
		"locationId":     fleetID,
		"operatingAmbit": "space",
		"slot":           1,
	})
	if err := (structHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
		t.Fatalf("seed struct-on-fleet %s: %v", structID, err)
	}
}

// moveFleet relocates a already-seeded fleet to planetID and flushes.
func moveFleet(t *testing.T, tx pgx.Tx, bc BlockContext, fleetID, planetID string) {
	t.Helper()
	ctx := context.Background()
	raw := mustJSON(t, map[string]any{
		"id":                   fleetID,
		"owner":                "structs1owner",
		"locationType":         "planet",
		"locationId":           planetID,
		"status":               "docked",
		"locationListForward":  "",
		"locationListBackward": "",
		"spaceSlots":           0,
		"airSlots":             0,
		"landSlots":            0,
		"waterSlots":           0,
	})
	if err := (fleetHandler{}).Handle(ctx, tx, bc, raw); err != nil {
		t.Fatalf("move fleet %s to %s: %v", fleetID, planetID, err)
	}
	flushBuf(t, ctx, tx, bc)
}

// TestPhase6_Fleet_CarriedPlanetaryDefensePresence walks a full raid round
// trip for a defender that rides a fleet while protecting a struct on the
// fleet's home planet: one struct_defense_remove when the fleet leaves
// home, silence while it hops between enemy planets, one
// struct_defense_add when it returns. structs.struct_defender is never
// touched — these are presence signals, not relationship changes.
func TestPhase6_Fleet_CarriedPlanetaryDefensePresence(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(720010)

		seedPlanetForActivity(t, tx, testFleetDefHome, "structs1owner")
		seedPlanetForActivity(t, tx, testFleetDefEnemyA, "structs1owner")
		seedPlanetForActivity(t, tx, testFleetDefEnemyB, "structs1owner")
		seedFleetAt(t, tx, testFleetDefFleet, testFleetDefHome, "docked")
		seedStructAt(t, tx, testFleetDefProtected, testFleetDefProtectedIndex, testFleetDefHome)
		seedStructOnFleet(t, tx, testFleetDefDefender, testFleetDefDefenderIndex, testFleetDefFleet)

		if err := (structDefenderHandler{}).Handle(ctx, tx, bc, mustJSON(t, map[string]any{
			"defendingStructId": testFleetDefDefender,
			"protectedStructId": testFleetDefProtected,
		})); err != nil {
			t.Fatalf("seed defender: %v", err)
		}
		// Guard the premise: if the upsert didn't flag this planetary the
		// emit query matches nothing and every assertion below passes
		// vacuously.
		var planetary bool
		if err := tx.QueryRow(ctx,
			`SELECT is_planetary FROM structs.struct_defender WHERE defending_struct_id=$1`,
			testFleetDefDefender).Scan(&planetary); err != nil {
			t.Fatalf("read is_planetary: %v", err)
		}
		if !planetary {
			t.Fatalf("seeded defense has is_planetary=false; want true")
		}

		// Leave home: remove anchored on home, nothing on the destination.
		moveFleet(t, tx, bc, testFleetDefFleet, testFleetDefEnemyA)
		if got := countPlanetActivity(t, tx, testFleetDefHome, "struct_defense_remove"); got != 1 {
			t.Fatalf("struct_defense_remove on home = %d; want 1", got)
		}
		if got := countPlanetActivity(t, tx, testFleetDefEnemyA, "struct_defense_add"); got != 0 {
			t.Errorf("struct_defense_add on enemy A = %d; want 0", got)
		}

		// Detail must be indistinguishable from a chain-driven defense
		// event: the same two keys and nothing else, so the webapp needs
		// no change to react to it.
		var defID, protID string
		var keys int
		if err := tx.QueryRow(ctx,
			`SELECT detail->>'defender_struct_id', detail->>'protected_struct_id',
			        (SELECT count(*) FROM jsonb_object_keys(detail))
			   FROM structs.planet_activity
			  WHERE planet_id=$1 AND category='struct_defense_remove'`,
			testFleetDefHome).Scan(&defID, &protID, &keys); err != nil {
			t.Fatalf("read remove detail: %v", err)
		}
		if defID != testFleetDefDefender {
			t.Errorf("remove defender = %q want %q", defID, testFleetDefDefender)
		}
		if protID != testFleetDefProtected {
			t.Errorf("remove protected = %q want %q", protID, testFleetDefProtected)
		}
		if keys != 2 {
			t.Errorf("remove detail has %d keys; want exactly 2 (chain-driven shape)", keys)
		}

		// Enemy A to enemy B: the protected struct is on neither planet,
		// so both sides match zero rows. This is what keeps a multi-hop
		// raid from spamming defense events.
		moveFleet(t, tx, bc, testFleetDefFleet, testFleetDefEnemyB)
		for _, planet := range []string{testFleetDefEnemyA, testFleetDefEnemyB} {
			for _, cat := range []string{"struct_defense_remove", "struct_defense_add"} {
				if got := countPlanetActivity(t, tx, planet, cat); got != 0 {
					t.Errorf("%s on %s after enemy-to-enemy hop = %d; want 0", cat, planet, got)
				}
			}
		}
		if got := countPlanetActivity(t, tx, testFleetDefHome, "struct_defense_remove"); got != 1 {
			t.Errorf("home struct_defense_remove after hop = %d; want 1 (unchanged)", got)
		}

		// Home again: one add, and no second remove.
		moveFleet(t, tx, bc, testFleetDefFleet, testFleetDefHome)
		if got := countPlanetActivity(t, tx, testFleetDefHome, "struct_defense_add"); got != 1 {
			t.Fatalf("struct_defense_add on home = %d; want 1", got)
		}
		if got := countPlanetActivity(t, tx, testFleetDefHome, "struct_defense_remove"); got != 1 {
			t.Errorf("home struct_defense_remove total = %d; want 1", got)
		}
		var addDef, addProt string
		if err := tx.QueryRow(ctx,
			`SELECT detail->>'defender_struct_id', detail->>'protected_struct_id'
			   FROM structs.planet_activity
			  WHERE planet_id=$1 AND category='struct_defense_add'`,
			testFleetDefHome).Scan(&addDef, &addProt); err != nil {
			t.Fatalf("read add detail: %v", err)
		}
		if addDef != testFleetDefDefender || addProt != testFleetDefProtected {
			t.Errorf("add detail = (%q, %q) want (%q, %q)",
				addDef, addProt, testFleetDefDefender, testFleetDefProtected)
		}

		// The relationship itself is untouched by the round trip.
		var stillThere bool
		if err := tx.QueryRow(ctx,
			`SELECT is_planetary FROM structs.struct_defender WHERE defending_struct_id=$1`,
			testFleetDefDefender).Scan(&stillThere); err != nil {
			t.Fatalf("defender row missing after round trip: %v", err)
		}
		if !stillThere {
			t.Errorf("is_planetary flipped to false; the move must not write struct_defender")
		}
	})
}

// TestPhase6_Fleet_SameFleetDefenseSilentOnMove covers the shape most of
// the live rows have: defender and protected ride the same fleet, so they
// travel together and a move must emit nothing.
func TestPhase6_Fleet_SameFleetDefenseSilentOnMove(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(720011)

		const (
			home      = "2-990975"
			away      = "2-990976"
			fleetID   = "3-990975"
			defender  = "5-990975"
			protected = "5-990976"
		)
		seedPlanetForActivity(t, tx, home, "structs1owner")
		seedPlanetForActivity(t, tx, away, "structs1owner")
		seedFleetAt(t, tx, fleetID, home, "docked")
		seedStructOnFleet(t, tx, defender, 990975, fleetID)
		seedStructOnFleet(t, tx, protected, 990976, fleetID)

		if err := (structDefenderHandler{}).Handle(ctx, tx, bc, mustJSON(t, map[string]any{
			"defendingStructId": defender,
			"protectedStructId": protected,
		})); err != nil {
			t.Fatalf("seed defender: %v", err)
		}

		moveFleet(t, tx, bc, fleetID, away)
		for _, planet := range []string{home, away} {
			for _, cat := range []string{"struct_defense_remove", "struct_defense_add"} {
				if got := countPlanetActivity(t, tx, planet, cat); got != 0 {
					t.Errorf("%s on %s = %d; want 0 (same-fleet defense travels with the fleet)",
						cat, planet, got)
				}
			}
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

		seedPlanetForActivity(t, tx, "2-999040", "structs1owner")
		seedStructAt(t, tx, "5-999040", 999040, "2-999040")

		raw := mustJSON(t, map[string]any{
			"attributeId": "1-5-999040",
			"value":       "3",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct_attribute: %v", err)
		}
		flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-999040", "struct_status"); got != 1 {
			t.Errorf("struct_status emit = %d; want 1", got)
		}
	})
}

func TestPhase6_StructAttr_BlockStartOreMineDoesNotEmit(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(730002)

		seedPlanetForActivity(t, tx, "2-999041", "structs1owner")
		seedStructAt(t, tx, "5-999041", 999041, "2-999041")

		raw := mustJSON(t, map[string]any{
			"attributeId": "3-5-999041",
			"value":       "100",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct_attribute: %v", err)
		}
		flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-999041", "struct_block_ore_mine_start"); got != 0 {
			t.Errorf("struct_block_ore_mine_start emit = %d; want 0 (clocks moved to planet attr 12)", got)
		}
	})
}

func TestPhase6_StructAttr_BlockStartOreRefineDoesNotEmit(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(730005)

		seedPlanetForActivity(t, tx, "2-999045", "structs1owner")
		seedStructAt(t, tx, "5-999045", 999045, "2-999045")

		raw := mustJSON(t, map[string]any{
			"attributeId": "4-5-999045",
			"value":       "200",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("struct_attribute: %v", err)
		}
		flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-999045", "struct_block_ore_refine_start"); got != 0 {
			t.Errorf("struct_block_ore_refine_start emit = %d; want 0 (clocks moved to planet attr 13)", got)
		}
	})
}

func TestPhase6_StructAttr_ProtectedIndexDeleteEmitsDefenseRemove(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(730003)

		// Planet hosts the PROTECTED struct (5-999042).
		seedPlanetForActivity(t, tx, "2-999042", "structs1owner")
		seedStructAt(t, tx, "5-999042", 999042, "2-999042")
		// Defender struct lives elsewhere — the activity anchors on the
		// protected struct's planet, not the defender's.
		seedPlanetForActivity(t, tx, "2-999043", "structs1owner")
		seedStructAt(t, tx, "5-999043", 999043, "2-999043")

		// Set defender protecting 999042.
		setup := mustJSON(t, map[string]any{
			"attributeId": "5-5-999043-0",
			"value":       "999042",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, setup); err != nil {
			t.Fatalf("setup: %v", err)
		}
		flushBuf(t, ctx, tx, bc)
		// Sanity: defense_add fired on protected planet.
		if got := countPlanetActivity(t, tx, "2-999042", "struct_defense_add"); got != 1 {
			t.Fatalf("setup struct_defense_add = %d; want 1", got)
		}

		// Now delete the attribute → struct_defense_remove on the
		// protected struct's planet. This exercises the SQL-trigger
		// dead-code DELETE branch we fixed.
		clear := mustJSON(t, map[string]any{
			"attributeId": "5-5-999043-0",
			"value":       "",
		})
		if err := (structAttributeHandler{}).Handle(ctx, tx, bc, clear); err != nil {
			t.Fatalf("delete: %v", err)
		}
		flushBuf(t, ctx, tx, bc)
		if got := countPlanetActivity(t, tx, "2-999042", "struct_defense_remove"); got != 1 {
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

		seedPlanetForActivity(t, tx, "2-999044", "structs1owner")
		seedStructAt(t, tx, "5-999044", 999044, "2-999044")

		raw := mustJSON(t, map[string]any{
			"attributeId": "1-5-999044",
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
		if got := countPlanetActivity(t, tx, "2-999044", "struct_status"); got != 1 {
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
