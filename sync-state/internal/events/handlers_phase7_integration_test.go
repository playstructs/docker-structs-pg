// Integration tests for the player + infusion derivation ports:
//   - playerHandler: UPDATE_ADDRESS_GUILD_ID port — propagates
//     player.guild_id changes to structs.player_address.
//   - infusionHandler: INFUSION_LEDGER_ENTRY port — emits paired
//     ualpha/ualpha.infused ledger rows for the full fuel_p on INSERT
//     and the delta on UPDATE.
//
// Triggers are suppressed via session_replication_role=replica so Go's
// derivation runs in isolation (no double-writes from any still-installed
// PG triggers in dev fixtures).
package events

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// infusionBctx returns a BlockContext for infusion-ledger tests. Kept
// as a thin wrapper over fixedBctx since the historical OwnInfusionLedger
// gate was removed when sync-state took unconditional ownership.
func infusionBctx(height int64) BlockContext {
	return fixedBctx(height)
}

// -------- Phase 7a: UPDATE_ADDRESS_GUILD_ID --------

// Seed a player_address row directly (lighter than running the full
// addressAssociationHandler chain for this focused test).
func seedPlayerAddress(t *testing.T, tx pgx.Tx, address, playerID, guildID string) {
	t.Helper()
	ctx := context.Background()
	var gid any
	if guildID == "" {
		gid = nil
	} else {
		gid = guildID
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO structs.player_address (address, player_id, guild_id, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'active', NOW(), NOW())
		 ON CONFLICT (address) DO UPDATE SET player_id=EXCLUDED.player_id, guild_id=EXCLUDED.guild_id`,
		address, playerID, gid)
	if err != nil {
		t.Fatalf("seed player_address: %v", err)
	}
}

func TestPhase7_PlayerGuildChange_PropagatesToAddresses(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(800001)

		playerID := "1-8001"
		// First: seed the player at guild=A
		first := mustJSON(t, map[string]any{
			"id":      playerID,
			"index":   8001,
			"guildId": "4-A",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bc, first); err != nil {
			t.Fatalf("player insert: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		// Seed two addresses pointing at the player with guild A.
		seedPlayerAddress(t, tx, "structs1addr1", playerID, "4-A")
		seedPlayerAddress(t, tx, "structs1addr2", playerID, "4-A")

		// Now update player to guild B.
		second := mustJSON(t, map[string]any{
			"id":      playerID,
			"index":   8001,
			"guildId": "4-B",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bc, second); err != nil {
			t.Fatalf("player update: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Both addresses should now have guild B.
		var addrs []string
		rows, err := tx.Query(ctx,
			`SELECT address FROM structs.player_address
			 WHERE player_id=$1 AND guild_id=$2 ORDER BY address`,
			playerID, "4-B")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			addrs = append(addrs, a)
		}
		rows.Close()
		if len(addrs) != 2 {
			t.Errorf("expected 2 addresses with guild=4-B; got %d (%v)", len(addrs), addrs)
		}
	})
}

func TestPhase7_PlayerInsertOnly_NoPropagation(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(800002)

		playerID := "1-8002"
		// Insert player. Seed an address with a STALE guild_id —
		// matches SQL trigger behavior: AFTER UPDATE only, so an
		// INSERT does NOT propagate.
		seedPlayerAddress(t, tx, "structs1stale", playerID, "4-OLD")
		raw := mustJSON(t, map[string]any{
			"id":      playerID,
			"index":   8002,
			"guildId": "4-NEW",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("player: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var got string
		err := tx.QueryRow(ctx,
			`SELECT guild_id FROM structs.player_address WHERE address='structs1stale'`).Scan(&got)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if got != "4-OLD" {
			t.Errorf("address guild on first insert = %q; want unchanged (4-OLD)", got)
		}
	})
}

func TestPhase7_PlayerNoGuildChange_NoPropagation(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := derivBctx(800003)

		playerID := "1-8003"
		first := mustJSON(t, map[string]any{
			"id":      playerID,
			"index":   8003,
			"guildId": "4-X",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bc, first); err != nil {
			t.Fatalf("insert: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		// Use a NULL-guild address — should stay NULL even after
		// a no-op player update.
		seedPlayerAddress(t, tx, "structs1nullguild", playerID, "")

		second := mustJSON(t, map[string]any{
			"id":             playerID,
			"index":          8003,
			"guildId":        "4-X",
			"primaryAddress": "structs1someother", // touches a different col
		})
		if err := (playerHandler{}).Handle(ctx, tx, bc, second); err != nil {
			t.Fatalf("update: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var guild *string
		_ = tx.QueryRow(ctx,
			`SELECT guild_id FROM structs.player_address WHERE address='structs1nullguild'`).Scan(&guild)
		if guild != nil {
			t.Errorf("address guild = %q; want NULL (guild_id unchanged so no propagation)", *guild)
		}
	})
}

// -------- Phase 7b: INFUSION_LEDGER_ENTRY --------

func TestPhase7_Infusion_StructInsertWritesLedgerPair(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := infusionBctx(810001)

		raw := mustJSON(t, map[string]any{
			"destinationId":   "5-81001",
			"address":         "structs1infuser1",
			"destinationType": "struct",
			"playerId":        "1-1",
			"fuel":            "1000000",
		})
		if err := (infusionHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("infusion: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var debitAmount, creditAmount int64
		err := tx.QueryRow(ctx,
			`SELECT
			   COALESCE(SUM(CASE WHEN direction='debit'  THEN amount_p::bigint END), 0),
			   COALESCE(SUM(CASE WHEN direction='credit' THEN amount_p::bigint END), 0)
			 FROM structs.ledger
			 WHERE block_height=$1 AND address=$2 AND action='infused'`,
			int64(810001), "structs1infuser1").Scan(&debitAmount, &creditAmount)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if debitAmount != 1000000 || creditAmount != 1000000 {
			t.Errorf("amounts: debit=%d credit=%d; want both = 1000000", debitAmount, creditAmount)
		}

		// Check denoms are correct (ualpha + ualpha.infused).
		denoms := map[string]int{}
		rows, err := tx.Query(ctx,
			`SELECT denom FROM structs.ledger WHERE block_height=$1 AND address=$2 AND action='infused'`,
			int64(810001), "structs1infuser1")
		if err != nil {
			t.Fatalf("denoms query: %v", err)
		}
		for rows.Next() {
			var d string
			_ = rows.Scan(&d)
			denoms[d]++
		}
		rows.Close()
		if denoms["ualpha"] != 1 || denoms["ualpha.infused"] != 1 {
			t.Errorf("denoms = %v; want {ualpha:1, ualpha.infused:1}", denoms)
		}
	})
}

func TestPhase7_Infusion_UpdateWritesDelta(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := infusionBctx(810002)

		base := map[string]any{
			"destinationId":   "5-81002",
			"address":         "structs1infuser2",
			"destinationType": "struct",
			"playerId":        "1-1",
			"fuel":            "500000",
		}
		if err := (infusionHandler{}).Handle(ctx, tx, bc, mustJSON(t, base)); err != nil {
			t.Fatalf("first infusion: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Bump fuel — delta = 700000-500000 = 200000.
		base["fuel"] = "700000"
		if err := (infusionHandler{}).Handle(ctx, tx, bc, mustJSON(t, base)); err != nil {
			t.Fatalf("second infusion: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		var debits []int64
		rows, err := tx.Query(ctx,
			`SELECT amount_p::bigint FROM structs.ledger
			 WHERE address=$1 AND action='infused' AND direction='debit'
			 ORDER BY ctid`,
			"structs1infuser2")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			debits = append(debits, v)
		}
		rows.Close()
		if len(debits) != 2 || debits[0] != 500000 || debits[1] != 200000 {
			t.Errorf("debit amounts = %v; want [500000, 200000] (full then delta)", debits)
		}
	})
}

func TestPhase7_Infusion_DefuseWritesNegativeDelta(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := infusionBctx(810003)

		base := map[string]any{
			"destinationId":   "5-81003",
			"address":         "structs1infuser3",
			"destinationType": "struct",
			"playerId":        "1-1",
			"fuel":            "800000",
		}
		if err := (infusionHandler{}).Handle(ctx, tx, bc, mustJSON(t, base)); err != nil {
			t.Fatalf("first: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Defuse half.
		base["fuel"] = "400000"
		if err := (infusionHandler{}).Handle(ctx, tx, bc, mustJSON(t, base)); err != nil {
			t.Fatalf("defuse: %v", err)
		}
			flushBuf(t, ctx, tx, bc)

		// Second debit row should be -400000.
		var amounts []int64
		rows, err := tx.Query(ctx,
			`SELECT amount_p::bigint FROM structs.ledger
			 WHERE address=$1 AND action='infused' AND direction='debit'
			 ORDER BY ctid`,
			"structs1infuser3")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			amounts = append(amounts, v)
		}
		rows.Close()
		if len(amounts) != 2 || amounts[1] != -400000 {
			t.Errorf("amounts = %v; want second row = -400000 (defusing delta)", amounts)
		}
	})
}

func TestPhase7_Infusion_NoOpUpdateSkipsLedger(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := infusionBctx(810004)

		base := mustJSON(t, map[string]any{
			"destinationId":   "5-81004",
			"address":         "structs1infuser4",
			"destinationType": "struct",
			"playerId":        "1-1",
			"fuel":            "123",
		})
		if err := (infusionHandler{}).Handle(ctx, tx, bc, base); err != nil {
			t.Fatalf("first: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		if err := (infusionHandler{}).Handle(ctx, tx, bc, base); err != nil {
			t.Fatalf("repeat: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.ledger
			 WHERE address=$1 AND action='infused' AND direction='debit'`,
			"structs1infuser4").Scan(&n)
		if n != 1 {
			t.Errorf("debit rows after repeated identical infusion = %d; want 1 (no delta = no second emit)", n)
		}
	})
}

func TestPhase7_Infusion_NonStructDestinationNoLedger(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := infusionBctx(810005)

		raw := mustJSON(t, map[string]any{
			"destinationId":   "8-81005",
			"address":         "structs1infuser5",
			"destinationType": "guild", // not 'struct'
			"playerId":        "1-1",
			"fuel":            "999",
		})
		if err := (infusionHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("infusion: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.ledger WHERE address=$1`, "structs1infuser5").Scan(&n)
		if n != 0 {
			t.Errorf("ledger rows for non-struct destination = %d; want 0", n)
		}
	})
}

// TestPhase7_Infusion_NonStruct_NoLedger confirms the outer
// destination_type='struct' gate: a non-struct destination (e.g. a
// fleet or a player) should NOT emit a ledger pair, matching the
// dropped SQL trigger's outer guard.
func TestPhase7_Infusion_NonStruct_NoLedger(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		suppressTriggers(t, tx)
		bc := fixedBctx(810006)
		raw := mustJSON(t, map[string]any{
			"destinationId":   "1-81006",
			"address":         "structs1infuser6",
			"destinationType": "player", // not 'struct' → outer gate skips
			"playerId":        "1-1",
			"fuel":            "111",
		})
		if err := (infusionHandler{}).Handle(ctx, tx, bc, raw); err != nil {
			t.Fatalf("infusion: %v", err)
		}
			flushBuf(t, ctx, tx, bc)
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.ledger WHERE address=$1`, "structs1infuser6").Scan(&n)
		if n != 0 {
			t.Errorf("ledger rows for non-struct destination = %d; want 0", n)
		}
	})
}
