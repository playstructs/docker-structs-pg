// Integration tests for the Phase 2 entity handlers. Each test uses a
// real local Postgres (the same structs-pg fixture sync-state runs
// against in dev) and rolls back its work in the per-test transaction
// so the tests are idempotent and don't pollute the dev DB.
//
// Set INTEGRATION_DATABASE_URL to opt in. Skips silently otherwise so
// `go test ./...` stays green on machines without a local PG.
//
//	INTEGRATION_DATABASE_URL=postgres://structs@localhost:5432/structs?sslmode=disable \
//	    go test ./internal/events/ -run TestHandler -v
package events

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
)

// connect opens a single connection to the integration PG. Skips the
// calling test if INTEGRATION_DATABASE_URL is unset.
func connect(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// inTx runs body inside a transaction that is always rolled back, so
// tests don't pollute the local DB.
func inTx(t *testing.T, conn *pgx.Conn, body func(tx pgx.Tx)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	body(tx)
}

// bctx returns a BlockContext suitable for handler tests. The Buf is
// pre-populated so the Phase-2 handlers (which push rows into
// bctx.Buf.*) don't nil-dereference. Tests that read from the
// append-only tables (ledger / defusion / planet_activity / stat_*)
// should flush the buffer before asserting — see flushBuf.
func bctx() BlockContext {
	return BlockContext{
		ChainID:   "test",
		Height:    100,
		BlockTime: time.Now(),
		TipHeight: 100,
		TxIndex:   -1, MsgIndex: -1, EventIndex: 0,
		Buf: buffers.New(),
	}
}

// flushBuf empties bc.Buf into tx via pgx.CopyFrom — call after Handle
// when the test reads back the buffered tables. Safe to call when bc.Buf
// is nil (no-op).
func flushBuf(t *testing.T, ctx context.Context, tx pgx.Tx, bc BlockContext) {
	t.Helper()
	if bc.Buf == nil {
		return
	}
	if err := bc.Buf.Flush(ctx, tx); err != nil {
		t.Fatalf("flush buffer: %v", err)
	}
}

// handle is the test-time analog of one router dispatch: it ensures bc
// has a Buf, runs h.Handle, then flushes the buffer so subsequent SELECTs
// see any rows the handler pushed into ledger / defusion / planet_activity
// / stat_*. Tests that need to share buffered rows across multiple
// Handle calls should use a `bc := bctx()` variable directly and call
// flushBuf themselves at the end.
func handle(t *testing.T, ctx context.Context, tx pgx.Tx, h Handler, bc BlockContext, raw json.RawMessage) {
	t.Helper()
	if bc.Buf == nil {
		bc.Buf = buffers.New()
	}
	if err := h.Handle(ctx, tx, bc, raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := bc.Buf.Flush(ctx, tx); err != nil {
		t.Fatalf("flush buffer: %v", err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// -- per-handler smoke: insert then re-insert; verify row + idempotency --

func TestHandler_Allocation(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":             "test-alloc-1",
			"type":           "power",
			"sourceObjectId": "src-1",
			"index":          7,
			"destinationId":  "dst-1",
			"creator":        "structs1creator",
			"controller":     "structs1ctrl",
		})
		if err := (allocationHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		var dest, ctrl string
		if err := tx.QueryRow(ctx,
			`SELECT destination_id, controller FROM structs.allocation WHERE id=$1`,
			"test-alloc-1").Scan(&dest, &ctrl); err != nil {
			t.Fatalf("select: %v", err)
		}
		if dest != "dst-1" || ctrl != "structs1ctrl" {
			t.Errorf("got dest=%q ctrl=%q", dest, ctrl)
		}
		// Re-running with identical payload should not bump updated_at
		// (IS DISTINCT FROM guard).
		var ts1 time.Time
		_ = tx.QueryRow(ctx, `SELECT updated_at FROM structs.allocation WHERE id=$1`, "test-alloc-1").Scan(&ts1)
		time.Sleep(10 * time.Millisecond)
		if err := (allocationHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("second insert: %v", err)
		}
		var ts2 time.Time
		_ = tx.QueryRow(ctx, `SELECT updated_at FROM structs.allocation WHERE id=$1`, "test-alloc-1").Scan(&ts2)
		if !ts1.Equal(ts2) {
			t.Errorf("idempotency broken: updated_at changed from %v to %v", ts1, ts2)
		}
	})
}

func TestHandler_Agreement(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":           "test-agr-1",
			"providerId":   "prov-1",
			"allocationId": "alloc-1",
			"capacity":     "1000",
			"startBlock":   "100",
			"endBlock":     "200",
			"creator":      "creator",
			"owner":        "owner-player",
		})
		if err := (agreementHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var cap, start, end int64
		_ = tx.QueryRow(ctx, `SELECT capacity, start_block, end_block FROM structs.agreement WHERE id=$1`, "test-agr-1").Scan(&cap, &start, &end)
		if cap != 1000 || start != 100 || end != 200 {
			t.Errorf("got cap=%d start=%d end=%d", cap, start, end)
		}
		// Sidecar
		var sidecarPlayer string
		_ = tx.QueryRow(ctx, `SELECT player_id FROM structs.player_object WHERE object_id=$1`, "test-agr-1").Scan(&sidecarPlayer)
		if sidecarPlayer != "owner-player" {
			t.Errorf("player_object: got %q want owner-player", sidecarPlayer)
		}
	})
}

func TestHandler_Guild(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":                                 "test-guild-1",
			"index":                              1,
			"endpoint":                           "http://example.com",
			"joinInfusionMinimum":                500,
			"joinInfusionMinimumBypassByRequest": "true",
			"joinInfusionMinimumBypassByInvite":  "false",
			"primaryReactorId":                   "r-1",
			"entrySubstationId":                  "s-1",
			"entryRank":                          "2",
			"creator":                            "creator",
			"owner":                              "owner-player",
			"name":                               "Test Guild",
			"pfp":                                "ipfs://abc",
		})
		if err := (guildHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var endpoint, name string
		var jim int64
		_ = tx.QueryRow(ctx, `SELECT endpoint, join_infusion_minimum_p FROM structs.guild WHERE id=$1`, "test-guild-1").Scan(&endpoint, &jim)
		if endpoint != "http://example.com" || jim != 500 {
			t.Errorf("guild row: endpoint=%q jim=%d", endpoint, jim)
		}
		_ = tx.QueryRow(ctx, `SELECT name FROM structs.guild_meta WHERE id=$1`, "test-guild-1").Scan(&name)
		if name != "Test Guild" {
			t.Errorf("guild_meta.name = %q want Test Guild", name)
		}
	})
}

func TestHandler_Infusion(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"destinationId":   "5-99",
			"address":         "structs1addr",
			"destinationType": "struct",
			"playerId":        "1-1",
			"fuel":            "1234567890",
			"defusing":        "0",
			"power":           "42",
			"ratio":           "100",
			"commission":      "5",
		})
		if err := (infusionHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var fuelP, defusingP int64
		var commission int64
		_ = tx.QueryRow(ctx,
			`SELECT fuel_p, defusing_p, commission FROM structs.infusion WHERE destination_id=$1 AND address=$2`,
			"5-99", "structs1addr").Scan(&fuelP, &defusingP, &commission)
		if fuelP != 1234567890 || defusingP != 0 || commission != 5 {
			t.Errorf("got fuel_p=%d defusing_p=%d commission=%d", fuelP, defusingP, commission)
		}
	})
}

func TestHandler_Fleet(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":                    "2-3",
			"owner":                 "1-1",
			"space":                 map[string]any{"slot1": "5-1"},
			"air":                   nil,
			"land":                  nil,
			"water":                 nil,
			"spaceSlots":            2,
			"airSlots":              0,
			"landSlots":             0,
			"waterSlots":            0,
			"locationType":          "planet",
			"locationId":            "3-7",
			"status":                "ONLINE",
			"locationListForward":   "",
			"locationListBackward":  "",
			"commandStruct":         "5-1",
		})
		if err := (fleetHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var owner, locID string
		var slots int64
		_ = tx.QueryRow(ctx, `SELECT owner, location_id, space_slots FROM structs.fleet WHERE id=$1`, "2-3").Scan(&owner, &locID, &slots)
		if owner != "1-1" || locID != "3-7" || slots != 2 {
			t.Errorf("fleet: owner=%q loc=%q slots=%d", owner, locID, slots)
		}
		// Verify map has space and the nulls for the others.
		var mapText string
		_ = tx.QueryRow(ctx, `SELECT map::text FROM structs.fleet WHERE id=$1`, "2-3").Scan(&mapText)
		if !contains(mapText, `"space"`) || !contains(mapText, `"air": null`) {
			t.Errorf("fleet.map: %s", mapText)
		}
	})
}

func TestHandler_Planet(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()

		// Production invariant: the NAME_PLANET trigger on structs.planet
		// inserts a planet_meta row keyed on (planet.id, player.guild_id);
		// planet_meta.guild_id is NOT NULL, so the owner player must exist
		// before the planet event lands. The chain always emits player
		// events before planet events for the same owner, so this is a
		// faithful test ordering rather than a sync-state bug.
		playerRaw := mustJSON(t, map[string]any{
			"id":      "1-1",
			"index":   1,
			"creator": "creator",
			"guildId": "",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bctx(), playerRaw); err != nil {
			t.Fatalf("prereq player insert: %v", err)
		}

		raw := mustJSON(t, map[string]any{
			"id":                "3-7",
			"maxOre":            5000,
			"creator":           "creator",
			"owner":             "1-1",
			"space":             nil,
			"air":               nil,
			"land":              nil,
			"water":             nil,
			"spaceSlots":        1,
			"airSlots":          2,
			"landSlots":         3,
			"waterSlots":        4,
			"status":            "ACTIVE",
			"locationListStart": "",
			"locationListEnd":   "",
			"name":              "Test Planet",
		})
		if err := (planetHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var owner string
		_ = tx.QueryRow(ctx, `SELECT owner FROM structs.planet WHERE id=$1`, "3-7").Scan(&owner)
		if owner != "1-1" {
			t.Errorf("planet.owner = %q want 1-1", owner)
		}
		// planet_meta seed comes from NAME_PLANET trigger (which runs since
		// we didn't disable it for tests); name override should land on
		// any row that the trigger seeded. Verify the chain name took.
		var name string
		err := tx.QueryRow(ctx, `SELECT name FROM structs.planet_meta WHERE id=$1 LIMIT 1`, "3-7").Scan(&name)
		if err == nil && name != "Test Planet" {
			t.Errorf("planet_meta.name = %q want Test Planet", name)
		}
	})
}

func TestHandler_Player(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":             "1-99",
			"index":          99,
			"creator":        "creator",
			"primaryAddress": "structs1addr",
			"guildId":        "",
			"substationId":   "",
			"planetId":       "",
			"fleetId":        "",
			"name":           "TestPlayer",
			"pfp":            "ipfs://pfp",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var primary string
		_ = tx.QueryRow(ctx, `SELECT primary_address FROM structs.player WHERE id=$1`, "1-99").Scan(&primary)
		if primary != "structs1addr" {
			t.Errorf("primary_address = %q", primary)
		}
		var username string
		_ = tx.QueryRow(ctx, `SELECT username FROM structs.player_meta WHERE id=$1`, "1-99").Scan(&username)
		if username != "TestPlayer" {
			t.Errorf("player_meta.username = %q want TestPlayer", username)
		}
		// self-mapping sidecar
		var selfMap string
		_ = tx.QueryRow(ctx, `SELECT player_id FROM structs.player_object WHERE object_id=$1`, "1-99").Scan(&selfMap)
		if selfMap != "1-99" {
			t.Errorf("self-map player_object = %q want 1-99", selfMap)
		}
	})
}

func TestHandler_Provider(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":                          "test-prov-1",
			"index":                       1,
			"substationId":                "s-1",
			"rate":                        map[string]any{"amount": "100", "denom": "uvert"},
			"accessPolicy":                "open",
			"capacityMinimum":             "1",
			"capacityMaximum":             "1000",
			"durationMinimum":             "10",
			"durationMaximum":             "10000",
			"providerCancellationPenalty": "5",
			"consumerCancellationPenalty": "10",
			"creator":                     "creator",
			"owner":                       "owner-player",
		})
		if err := (providerHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var amount int64
		var denom string
		_ = tx.QueryRow(ctx, `SELECT rate_amount, rate_denom FROM structs.provider WHERE id=$1`, "test-prov-1").Scan(&amount, &denom)
		if amount != 100 || denom != "uvert" {
			t.Errorf("rate: amount=%d denom=%q", amount, denom)
		}
	})
}

func TestHandler_Reactor(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":                "test-r-1",
			"validator":         "structsvaloper1abc",
			"guildId":           "g-1",
			"defaultCommission": "5",
			"owner":             "owner-player",
		})
		if err := (reactorHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var v string
		var comm int64
		_ = tx.QueryRow(ctx, `SELECT validator, default_commission FROM structs.reactor WHERE id=$1`, "test-r-1").Scan(&v, &comm)
		if v != "structsvaloper1abc" || comm != 5 {
			t.Errorf("reactor: validator=%q commission=%d", v, comm)
		}
	})
}

func TestHandler_Struct(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":             "5-42",
			"index":          42,
			"type":           1,
			"creator":        "creator",
			"owner":          "1-1",
			"locationType":   "planet",
			"locationId":     "3-7",
			"operatingAmbit": "LAND",
			"slot":           3,
		})
		if err := (structHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var loc, ambit string
		var slot int64
		var destroyed bool
		_ = tx.QueryRow(ctx,
			`SELECT location_id, operating_ambit, slot, is_destroyed FROM structs.struct WHERE id=$1`,
			"5-42").Scan(&loc, &ambit, &slot, &destroyed)
		if loc != "3-7" || ambit != "LAND" || slot != 3 || destroyed != false {
			t.Errorf("struct: loc=%q ambit=%q slot=%d destroyed=%v", loc, ambit, slot, destroyed)
		}
	})
}

func TestHandler_StructType(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Use a high id to avoid colliding with real struct_types.
		raw := mustJSON(t, map[string]any{
			"id":                                     9999,
			"type":                                   "TestType",
			"category":                               "TEST",
			"buildLimit":                             10,
			"buildDifficulty":                        100,
			"buildDraw":                              500,
			"maxHealth":                              1000,
			"passiveDraw":                            50,
			"possibleAmbit":                          15,
			"movable":                                true,
			"slotBound":                              false,
			"primaryWeapon":                          "BLASTER",
			"primaryWeaponControl":                   "MANUAL",
			"primaryWeaponCharge":                    0,
			"primaryWeaponAmbits":                    15,
			"primaryWeaponTargets":                   1,
			"primaryWeaponShots":                     1,
			"primaryWeaponDamage":                    10,
			"primaryWeaponBlockable":                 true,
			"primaryWeaponCounterable":               true,
			"primaryWeaponRecoilDamage":              0,
			"primaryWeaponShotSuccessRateNumerator":   1,
			"primaryWeaponShotSuccessRateDenominator": 1,
			"class":                                  "Command Ship",
		})
		if err := (structTypeHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var typ, class string
		var bdp, isCmd bool
		_ = tx.QueryRow(ctx, `SELECT type, class, movable, is_command FROM structs.struct_type WHERE id=$1`, 9999).Scan(&typ, &class, &bdp, &isCmd)
		if typ != "TestType" || class != "Command Ship" || !isCmd {
			t.Errorf("struct_type: type=%q class=%q isCmd=%v", typ, class, isCmd)
		}
		// build_draw_p check
		var bdraw int64
		_ = tx.QueryRow(ctx, `SELECT build_draw_p FROM structs.struct_type WHERE id=$1`, 9999).Scan(&bdraw)
		if bdraw != 500 {
			t.Errorf("build_draw_p = %d want 500", bdraw)
		}
	})
}

func TestHandler_Substation(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"id":      "test-sub-1",
			"owner":   "owner-player",
			"creator": "creator",
			"name":    "Test Sub",
			"pfp":     "ipfs://sub",
		})
		if err := (substationHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var owner, name, pfp string
		_ = tx.QueryRow(ctx, `SELECT owner, name, pfp FROM structs.substation WHERE id=$1`, "test-sub-1").Scan(&owner, &name, &pfp)
		if owner != "owner-player" || name != "Test Sub" || pfp != "ipfs://sub" {
			t.Errorf("substation: owner=%q name=%q pfp=%q", owner, name, pfp)
		}
	})
}

// contains is a tiny strings.Contains alias so we don't import strings
// just for one call site.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
