// Integration tests for Phase 5 handlers (activity / ledger). Opt-in via
// INTEGRATION_DATABASE_URL; rolls back the per-test transaction so the
// dev DB stays clean.
//
// These tests cover:
//   - oreMine / oreMigrate / oreTheft / alphaRefine — ledger row shape,
//     block_height/time from bctx, NUMERIC precision (alphaRefine multiplier)
//   - attack — planet_activity insert, GET_ACTIVITY_LOCATION_ID Go port
//     covering struct-on-planet, struct-on-fleet, and unknown-struct paths
//   - raid — planet_raid upsert + IS DISTINCT FROM guard on (fleet, status),
//     seized_ore-only update DOES NOT touch updated_at
//   - providerAddress / guildBankAddress — address_tag multi-row upserts
//   - time — no-op safety check
//   - nextPlanetActivitySeq — monotonic per-planet counter
package events

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
)

// fixedBctx returns a BlockContext with a pinned BlockTime so tests can
// assert exact equality on time-series columns. Buf is pre-allocated so
// Phase-2 handlers can append without a nil deref; callers that read
// from the buffered tables should flushBuf before asserting.
func fixedBctx(height int64) BlockContext {
	return BlockContext{
		ChainID:    "test",
		Height:     height,
		BlockTime:  time.Date(2026, 5, 20, 17, 0, 0, 0, time.UTC),
		TipHeight:  height,
		TxIndex:    -1,
		MsgIndex:   -1,
		EventIndex: 0,
		Buf:        buffers.New(),
	}
}

// -------- helpers --------

func TestNextPlanetActivitySeq_Monotonic(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		planet := "2-9001"

		// First call: 0 (matches SQL: VALUES($1, 0) inserts 0 with no UPDATE).
		got, err := nextPlanetActivitySeq(ctx, tx, planet)
		if err != nil {
			t.Fatalf("call 1: %v", err)
		}
		if got != 0 {
			t.Errorf("first call: got %d want 0", got)
		}
		// Subsequent calls: 1, 2, 3 …
		for want := 1; want <= 3; want++ {
			got, err := nextPlanetActivitySeq(ctx, tx, planet)
			if err != nil {
				t.Fatalf("call %d: %v", want+1, err)
			}
			if got != want {
				t.Errorf("call %d: got %d want %d", want+1, got, want)
			}
		}

		// Different planet starts at its own 0.
		other, err := nextPlanetActivitySeq(ctx, tx, "2-9002")
		if err != nil {
			t.Fatalf("other planet: %v", err)
		}
		if other != 0 {
			t.Errorf("other planet first call: got %d want 0", other)
		}
	})
}

// -------- ore_mine --------

func TestHandler_OreMine_WritesLedgerRow(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(123456)
		raw := mustJSON(t, map[string]any{
			"primaryAddress": "structs1miner",
			"amount":         "42",
		})
		if err := (oreMineHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var addr, action, direction, denom string
		var amountP int64
		var blockHeight int64
		var rowTime time.Time
		var counterparty *string
		err := tx.QueryRow(ctx,
			`SELECT address, counterparty, amount_p::bigint, block_height, time, action::text, direction::text, denom
			 FROM structs.ledger WHERE address=$1 AND action='mined' AND block_height=$2`,
			"structs1miner", int64(123456)).Scan(&addr, &counterparty, &amountP, &blockHeight, &rowTime, &action, &direction, &denom)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if action != "mined" || direction != "credit" || denom != "ore" {
			t.Errorf("got action=%q dir=%q denom=%q; want mined/credit/ore", action, direction, denom)
		}
		if counterparty != nil {
			t.Errorf("counterparty = %q; want NULL", *counterparty)
		}
		if amountP != 42 {
			t.Errorf("amount_p = %d; want 42", amountP)
		}
		if !rowTime.Equal(bc.BlockTime) {
			t.Errorf("time = %s; want %s (from bctx.BlockTime)", rowTime, bc.BlockTime)
		}
	})
}

// -------- ore_migrate --------

func TestHandler_OreMigrate_WritesPairedLedgerRows(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(123457)
		raw := mustJSON(t, map[string]any{
			"primaryAddress":    "structs1new",
			"oldPrimaryAddress": "structs1old",
			"amount":            "100",
		})
		if err := (oreMigrateHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Credit on new
		var counter string
		var amount int64
		_ = tx.QueryRow(ctx,
			`SELECT counterparty, amount_p::bigint FROM structs.ledger
			 WHERE address='structs1new' AND action='migrated' AND direction='credit' AND block_height=$1`,
			int64(123457)).Scan(&counter, &amount)
		if counter != "structs1old" || amount != 100 {
			t.Errorf("credit row: counter=%q amount=%d", counter, amount)
		}

		// Debit on old
		_ = tx.QueryRow(ctx,
			`SELECT counterparty, amount_p::bigint FROM structs.ledger
			 WHERE address='structs1old' AND action='migrated' AND direction='debit' AND block_height=$1`,
			int64(123457)).Scan(&counter, &amount)
		if counter != "structs1new" || amount != 100 {
			t.Errorf("debit row: counter=%q amount=%d", counter, amount)
		}
	})
}

// -------- ore_theft --------

func TestHandler_OreTheft_WritesPairedLedgerRows(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(123458)
		raw := mustJSON(t, map[string]any{
			"thiefPrimaryAddress":  "structs1thief",
			"victimPrimaryAddress": "structs1victim",
			"amount":               "7",
		})
		if err := (oreTheftHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		var action, counter string
		var amount int64
		// Thief side: seized credit
		_ = tx.QueryRow(ctx,
			`SELECT action::text, counterparty, amount_p::bigint FROM structs.ledger
			 WHERE address='structs1thief' AND direction='credit' AND block_height=$1`,
			int64(123458)).Scan(&action, &counter, &amount)
		if action != "seized" || counter != "structs1victim" || amount != 7 {
			t.Errorf("thief row: action=%q counter=%q amount=%d", action, counter, amount)
		}
		// Victim side: forfeited debit
		_ = tx.QueryRow(ctx,
			`SELECT action::text, counterparty, amount_p::bigint FROM structs.ledger
			 WHERE address='structs1victim' AND direction='debit' AND block_height=$1`,
			int64(123458)).Scan(&action, &counter, &amount)
		if action != "forfeited" || counter != "structs1thief" || amount != 7 {
			t.Errorf("victim row: action=%q counter=%q amount=%d", action, counter, amount)
		}
	})
}

// -------- alpha_refine --------

func TestHandler_AlphaRefine_OreDebitAndUalphaCredit(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(123459)
		raw := mustJSON(t, map[string]any{
			"primaryAddress": "structs1refiner",
			"amount":         "5",
		})
		if err := (alphaRefineHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Ore debit: amount_p = 5
		var amount string
		_ = tx.QueryRow(ctx,
			`SELECT amount_p::text FROM structs.ledger
			 WHERE address='structs1refiner' AND action='refined' AND direction='debit' AND denom='ore' AND block_height=$1`,
			int64(123459)).Scan(&amount)
		if amount != "5" {
			t.Errorf("ore debit amount_p = %q; want 5", amount)
		}

		// Ualpha credit: amount_p = 5 * 1_000_000 = 5_000_000
		_ = tx.QueryRow(ctx,
			`SELECT amount_p::text FROM structs.ledger
			 WHERE address='structs1refiner' AND action='refined' AND direction='credit' AND denom='ualpha' AND block_height=$1`,
			int64(123459)).Scan(&amount)
		if amount != "5000000" {
			t.Errorf("ualpha credit amount_p = %q; want 5000000", amount)
		}
	})
}

// -------- attack --------

func TestHandler_Attack_StructOnPlanet_WritesPlanetActivity(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(200001)

		// Seed an attacker struct sitting directly on a planet (not on a fleet).
		structRaw := mustJSON(t, map[string]any{
			"id": "5-50001", "index": 50001, "type": 1, "creator": "c",
			"owner":          "1-1",
			"locationType":   "planet",
			"locationId":     "2-77",
			"operatingAmbit": "LAND", "slot": 1,
		})
		if err := (structHandler{}).Handle(ctx, tx, bc, structRaw); err != nil {
			t.Fatalf("seed struct: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		raw := mustJSON(t, map[string]any{
			"attackerStructId": "5-50001",
			"defenderStructId": "5-99999",
			"damage":           42,
		})
		if err := (attackHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("attack: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		var seq int
		var planet, category string
		var detail string
		_ = tx.QueryRow(ctx,
			`SELECT seq, planet_id, category::text, detail::text
			 FROM structs.planet_activity
			 WHERE planet_id='2-77' AND category='struct_attack'
			 ORDER BY time DESC LIMIT 1`).Scan(&seq, &planet, &category, &detail)
		if planet != "2-77" || category != "struct_attack" {
			t.Errorf("row: planet=%q category=%q", planet, category)
		}
		if seq != 0 {
			t.Errorf("seq = %d; want 0 (first activity for this planet)", seq)
		}
		// PG's jsonb stringifier may add a space after the colon; match either form.
		if detail == "" || !(contains(detail, `"attackerStructId":"5-50001"`) || contains(detail, `"attackerStructId": "5-50001"`)) {
			t.Errorf("detail jsonb missing attackerStructId: %s", detail)
		}
	})
}

func TestHandler_Attack_StructOnFleet_ResolvesPlanet(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(200002)

		// Seed a fleet at a planet, and a struct on that fleet.
		fleetRaw := mustJSON(t, map[string]any{
			"id": "9-30001", "index": 30001, "type": "carrier",
			"owner":          "1-1",
			"locationType":   "planet",
			"locationId":     "2-88",
			"operatingAmbit": "SPACE",
		})
		if err := (fleetHandler{}).Handle(ctx, tx, bc, fleetRaw); err != nil {
			t.Fatalf("seed fleet: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		structRaw := mustJSON(t, map[string]any{
			"id": "5-50002", "index": 50002, "type": 1, "creator": "c",
			"owner":          "1-1",
			"locationType":   "fleet",
			"locationId":     "9-30001",
			"operatingAmbit": "SPACE", "slot": 1,
		})
		if err := (structHandler{}).Handle(ctx, tx, bc, structRaw); err != nil {
			t.Fatalf("seed struct: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		raw := mustJSON(t, map[string]any{"attackerStructId": "5-50002"})
		if err := (attackHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("attack: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		var planet string
		_ = tx.QueryRow(ctx,
			`SELECT planet_id FROM structs.planet_activity
			 WHERE category='struct_attack' AND planet_id='2-88' LIMIT 1`).Scan(&planet)
		if planet != "2-88" {
			t.Errorf("expected planet resolved via fleet to 2-88, got %q", planet)
		}
	})
}

func TestHandler_Attack_UnknownStruct_SkipsCleanly(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(200003)
		// No struct seeded — planet resolves to "". The Go handler
		// short-circuits BEFORE the seq lookup and the NOT NULL
		// planet_activity insert. Net effect: no error, no row written.
		raw := mustJSON(t, map[string]any{"attackerStructId": "5-doesnotexist"})
		if err := (attackHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("attack should not error on unknown struct: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var rows int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.planet_activity
			 WHERE category='struct_attack'
			   AND detail::text LIKE '%5-doesnotexist%'`).Scan(&rows)
		if rows != 0 {
			t.Errorf("expected 0 planet_activity rows for unknown struct, got %d", rows)
		}
	})
}

// -------- raid --------

func TestHandler_Raid_InsertThenSeizedOreUpdate(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(300001)

		// Insert: status="started", no seized_ore yet.
		ins := mustJSON(t, map[string]any{
			"fleetId":  "9-1",
			"planetId": "2-100",
			"status":   "started",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, ins); err != nil {
			t.Fatalf("insert: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var status string
		var ore *string
		_ = tx.QueryRow(ctx,
			`SELECT status, seized_ore::text FROM structs.planet_raid WHERE planet_id=$1`,
			"2-100").Scan(&status, &ore)
		if status != "started" || ore != nil {
			t.Errorf("after insert: status=%q ore=%v", status, ore)
		}

		// Update: status="completed", seized_ore=42 → status changed so
		// IS DISTINCT FROM guard fires; seized_ore lands.
		upd := mustJSON(t, map[string]any{
			"fleetId":    "9-1",
			"planetId":   "2-100",
			"status":     "completed",
			"seized_ore": "42",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, upd); err != nil {
			t.Fatalf("update: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		_ = tx.QueryRow(ctx,
			`SELECT status, seized_ore::text FROM structs.planet_raid WHERE planet_id=$1`,
			"2-100").Scan(&status, &ore)
		if status != "completed" || ore == nil || *ore != "42" {
			t.Errorf("after status+ore update: status=%q ore=%v", status, ore)
		}
	})
}

func TestHandler_Raid_SeizedOreOnlyUpdate_SkippedByGuard(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		bc := fixedBctx(300002)

		// Seed with both fields set.
		ins := mustJSON(t, map[string]any{
			"fleetId": "9-2", "planetId": "2-101", "status": "completed", "seized_ore": "10",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, ins); err != nil {
			t.Fatalf("seed: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Now try to change ONLY seized_ore (same fleet, same status).
		// The IS DISTINCT FROM guard on (fleet, status) means this is a
		// no-op upsert — seized_ore stays at 10. This mirrors the SQL
		// handler's behavior exactly (intentional, even if surprising).
		upd := mustJSON(t, map[string]any{
			"fleetId": "9-2", "planetId": "2-101", "status": "completed", "seized_ore": "99",
		})
		if err := (raidHandler{}).Handle(ctx, tx, bc, upd); err != nil {
			t.Fatalf("ore-only update: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var ore string
		_ = tx.QueryRow(ctx, `SELECT seized_ore::text FROM structs.planet_raid WHERE planet_id=$1`, "2-101").Scan(&ore)
		if ore != "10" {
			t.Errorf("seized_ore = %q; want 10 (guard should have suppressed UPDATE)", ore)
		}
	})
}

// -------- provider_address --------

func TestHandler_ProviderAddress_WritesFourAddressTags(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"collateralPool": "structs1collat",
			"providerId":     "10-1",
			"earningPool":    "structs1earn",
		})
		if err := (providerAddressHandler{}).Handle(ctx, tx, fixedBctx(1), raw); err != nil {
			t.Fatalf("handle: %v", err)
		}

		rows := map[[2]string]string{
			{"structs1collat", "Type"}:       "Provider Collateral Pool",
			{"structs1collat", "ProviderId"}: "10-1",
			{"structs1earn", "Type"}:         "Provider Earning Pool",
			{"structs1earn", "ProviderId"}:   "10-1",
		}
		for k, wantEntry := range rows {
			var entry string
			err := tx.QueryRow(ctx,
				`SELECT entry FROM structs.address_tag WHERE address=$1 AND label=$2`,
				k[0], k[1]).Scan(&entry)
			if err != nil {
				t.Errorf("missing row addr=%s label=%s: %v", k[0], k[1], err)
				continue
			}
			if entry != wantEntry {
				t.Errorf("addr=%s label=%s: entry=%q want %q", k[0], k[1], entry, wantEntry)
			}
		}
	})
}

// -------- guild_bank_address --------

func TestHandler_GuildBankAddress_WritesTwoAddressTags(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"bankCollateralPool": "structs1bankpool",
			"guildId":            "0-7",
		})
		if err := (guildBankAddressHandler{}).Handle(ctx, tx, fixedBctx(1), raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
		var typeEntry, guildEntry string
		_ = tx.QueryRow(ctx,
			`SELECT entry FROM structs.address_tag WHERE address=$1 AND label='Type'`,
			"structs1bankpool").Scan(&typeEntry)
		_ = tx.QueryRow(ctx,
			`SELECT entry FROM structs.address_tag WHERE address=$1 AND label='GuildId'`,
			"structs1bankpool").Scan(&guildEntry)
		if typeEntry != "Bank Collateral Pool" {
			t.Errorf("Type entry = %q; want Bank Collateral Pool", typeEntry)
		}
		if guildEntry != "0-7" {
			t.Errorf("GuildId entry = %q; want 0-7", guildEntry)
		}
	})
}

// -------- time (no-op) --------

func TestHandler_Time_IsNoOp(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Snapshot before
		var beforeHeight int64
		_ = tx.QueryRow(ctx, `SELECT height FROM structs.current_block LIMIT 1`).Scan(&beforeHeight)

		raw := mustJSON(t, map[string]any{
			"blockHeight": strconv.FormatInt(beforeHeight+99999, 10),
			"blockTime":   time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err := (timeHandler{}).Handle(ctx, tx, fixedBctx(beforeHeight+99999), raw); err != nil {
			t.Fatalf("handle: %v", err)
		}

		// Height should be unchanged — timeHandler is a no-op.
		var afterHeight int64
		_ = tx.QueryRow(ctx, `SELECT height FROM structs.current_block LIMIT 1`).Scan(&afterHeight)
		if afterHeight != beforeHeight {
			t.Errorf("current_block.height changed (%d → %d); timeHandler should be a no-op", beforeHeight, afterHeight)
		}
	})
}

