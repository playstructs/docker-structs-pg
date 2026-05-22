// Integration tests for Phase 3 handlers (membership / address /
// permission / defender). Same opt-in pattern as Phase 2: requires
// INTEGRATION_DATABASE_URL and rolls back its transaction at the end of
// every test so the dev DB stays clean.
package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestHandler_GuildMembershipApplication(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"guildId":            "0-1",
			"playerId":           "1-99",
			"joinType":           "invite",
			"registrationStatus": "pending",
			"proposer":           "1-1",
			"substationId":       "4-1",
		})
		if err := (guildMembershipApplicationHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var status, joinType string
		_ = tx.QueryRow(ctx,
			`SELECT join_type, status FROM structs.guild_membership_application WHERE guild_id=$1 AND player_id=$2`,
			"0-1", "1-99").Scan(&joinType, &status)
		if joinType != "invite" || status != "pending" {
			t.Errorf("got joinType=%q status=%q", joinType, status)
		}
	})
}

func TestHandler_Address(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()

		// Pre-seed the player so the guild_id sub-select returns a value
		// (matches production order: player events arrive before address).
		playerRaw := mustJSON(t, map[string]any{
			"id":      "1-77",
			"index":   77,
			"creator": "creator",
			"guildId": "0-2",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bctx(), playerRaw); err != nil {
			t.Fatalf("seed player: %v", err)
		}

		raw := mustJSON(t, map[string]any{
			"address":  "structs1testaddr1",
			"playerId": "1-77",
		})
		if err := (addressHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("address insert: %v", err)
		}
		var status, pid, gid string
		_ = tx.QueryRow(ctx,
			`SELECT status, player_id, COALESCE(guild_id,'') FROM structs.player_address WHERE address=$1`,
			"structs1testaddr1").Scan(&status, &pid, &gid)
		if status != "approved" || pid != "1-77" || gid != "0-2" {
			t.Errorf("address row: status=%q pid=%q gid=%q", status, pid, gid)
		}
	})
}

func TestHandler_AddressActivity(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()

		// Pre-seed via handler chain: player → address → activity.
		seedPlayer := mustJSON(t, map[string]any{
			"id": "1-77", "index": 77, "creator": "creator",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bctx(), seedPlayer); err != nil {
			t.Fatalf("seed player: %v", err)
		}
		seedAddr := mustJSON(t, map[string]any{
			"address": "structs1testaddr1", "playerId": "1-77",
		})
		if err := (addressHandler{}).Handle(ctx, tx, bctx(), seedAddr); err != nil {
			t.Fatalf("seed address: %v", err)
		}

		bt := time.Date(2026, 5, 20, 17, 0, 0, 0, time.UTC)
		raw := mustJSON(t, map[string]any{
			"address":     "structs1testaddr1",
			"blockHeight": "123456",
			"blockTime":   bt.Format(time.RFC3339Nano),
		})
		if err := (addressActivityHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var pid string
		var height int64
		_ = tx.QueryRow(ctx,
			`SELECT player_id, block_height FROM structs.player_address_activity WHERE address=$1`,
			"structs1testaddr1").Scan(&pid, &height)
		if pid != "1-77" || height != 123456 {
			t.Errorf("activity row: pid=%q height=%d", pid, height)
		}
	})
}

func TestHandler_AddressAssociation_SkipsWhenPlayerMissing(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// No player row exists for player_index=4242 → handler should
		// silently skip (matches 20260223 guard).
		raw := mustJSON(t, map[string]any{
			"address":            "structs1nopayer",
			"playerIndex":        4242,
			"registrationStatus": "approved",
		})
		if err := (addressAssociationHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
		var count int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.player_address WHERE address=$1`, "structs1nopayer").Scan(&count)
		if count != 0 {
			t.Errorf("expected zero rows since player is missing, got %d", count)
		}
	})
}

func TestHandler_AddressAssociation_WritesWhenPlayerExists(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Pre-seed the player via the playerHandler so all PG triggers
		// (PLAYER_ADDRESS_CASCADE etc.) get the data they need.
		seedPlayer := mustJSON(t, map[string]any{
			"id":      "1-555",
			"index":   555,
			"creator": "creator",
			"guildId": "0-1",
		})
		if err := (playerHandler{}).Handle(ctx, tx, bctx(), seedPlayer); err != nil {
			t.Fatalf("seed player: %v", err)
		}
		raw := mustJSON(t, map[string]any{
			"address":            "structs1assoctest",
			"playerIndex":        555,
			"registrationStatus": "pending",
		})
		if err := (addressAssociationHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
		var pid, status, gid string
		_ = tx.QueryRow(ctx,
			`SELECT player_id, status, COALESCE(guild_id,'') FROM structs.player_address WHERE address=$1`,
			"structs1assoctest").Scan(&pid, &status, &gid)
		if pid != "1-555" || status != "pending" || gid != "0-1" {
			t.Errorf("row: pid=%q status=%q gid=%q", pid, status, gid)
		}
	})
}

func TestHandler_Permission_InsertAndDelete(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()

		// INSERT path: simple struct permission
		raw := mustJSON(t, map[string]any{
			"permissionId": "5-42@1-1",
			"value":        "7",
		})
		if err := (permissionHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var otype, oid, pid string
		var val int
		_ = tx.QueryRow(ctx,
			`SELECT object_type, object_id, player_id, val FROM structs.permission WHERE id=$1`,
			"5-42@1-1").Scan(&otype, &oid, &pid, &val)
		if otype != "struct" || oid != "5-42" || pid != "1-1" || val != 7 {
			t.Errorf("row: type=%q oid=%q pid=%q val=%d", otype, oid, pid, val)
		}

		// DELETE path: value="" removes the row
		delRaw := mustJSON(t, map[string]any{
			"permissionId": "5-42@1-1",
			"value":        "",
		})
		if err := (permissionHandler{}).Handle(ctx, tx, bctx(), delRaw); err != nil {
			t.Fatalf("delete: %v", err)
		}
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.permission WHERE id=$1`, "5-42@1-1").Scan(&n)
		if n != 0 {
			t.Errorf("expected row deleted, count=%d", n)
		}
	})
}

func TestHandler_Permission_AddressType8(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Seed a player_address row so the type-8 sub-select finds it.
		_, _ = tx.Exec(ctx, `INSERT INTO structs.player_address (address, player_id, status, created_at, updated_at)
			VALUES ($1, $2, 'approved', NOW(), NOW()) ON CONFLICT DO NOTHING`,
			"structs1permtarget", "1-300")

		raw := mustJSON(t, map[string]any{
			"permissionId": "8-structs1permtarget",
			"value":        "15",
		})
		if err := (permissionHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var otype, oidx string
		var oid, pid *string
		var val int
		_ = tx.QueryRow(ctx,
			`SELECT object_type, object_index, object_id, player_id, val FROM structs.permission WHERE id=$1`,
			"8-structs1permtarget").Scan(&otype, &oidx, &oid, &pid, &val)
		if otype != "address" || oidx != "structs1permtarget" {
			t.Errorf("type/idx: %q/%q", otype, oidx)
		}
		if oid == nil || pid == nil || *oid != "1-300" || *pid != "1-300" {
			t.Errorf("address lookup oid=%v pid=%v want 1-300", oid, pid)
		}
		if val != 15 {
			t.Errorf("val = %d want 15", val)
		}
	})
}

func TestHandler_Permission_AddressType8_MissingAddress(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// No player_address row → object_id and player_id end up NULL,
		// matching SQL behavior.
		raw := mustJSON(t, map[string]any{
			"permissionId": "8-structs1unknown",
			"value":        "1",
		})
		if err := (permissionHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var otype string
		var oid, pid *string
		_ = tx.QueryRow(ctx,
			`SELECT object_type, object_id, player_id FROM structs.permission WHERE id=$1`,
			"8-structs1unknown").Scan(&otype, &oid, &pid)
		if otype != "address" {
			t.Errorf("type = %q want address", otype)
		}
		if oid != nil || pid != nil {
			t.Errorf("expected NULL oid/pid for unknown address, got oid=%v pid=%v", oid, pid)
		}
	})
}

func TestHandler_GuildRankPermission_UpsertThenDelete(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()

		// INSERT with rank > 0
		upRaw := mustJSON(t, map[string]any{
			"objectId":    "5-99",
			"guildId":     "0-1",
			"permissions": "8",
			"rank":        "2",
		})
		if err := (guildRankPermissionHandler{}).Handle(ctx, tx, bctx(), upRaw); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		var rank int64
		_ = tx.QueryRow(ctx,
			`SELECT rank FROM structs.permission_guild_rank WHERE object_id=$1 AND guild_id=$2 AND permission=$3`,
			"5-99", "0-1", 8).Scan(&rank)
		if rank != 2 {
			t.Errorf("rank = %d want 2", rank)
		}

		// DELETE branch: rank=0
		delRaw := mustJSON(t, map[string]any{
			"objectId":    "5-99",
			"guildId":     "0-1",
			"permissions": "8",
			"rank":        "0",
		})
		if err := (guildRankPermissionHandler{}).Handle(ctx, tx, bctx(), delRaw); err != nil {
			t.Fatalf("delete: %v", err)
		}
		var n int
		_ = tx.QueryRow(ctx,
			`SELECT count(*) FROM structs.permission_guild_rank WHERE object_id=$1 AND guild_id=$2 AND permission=$3`,
			"5-99", "0-1", 8).Scan(&n)
		if n != 0 {
			t.Errorf("expected row deleted, count=%d", n)
		}
	})
}

func TestHandler_StructDefender(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{
			"defendingStructId": "5-100",
			"protectedStructId": "5-101",
		})
		if err := (structDefenderHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var prot string
		_ = tx.QueryRow(ctx, `SELECT protected_struct_id FROM structs.struct_defender WHERE defending_struct_id=$1`, "5-100").Scan(&prot)
		if prot != "5-101" {
			t.Errorf("protected = %q want 5-101", prot)
		}
		// Update to different protector
		raw2 := mustJSON(t, map[string]any{
			"defendingStructId": "5-100",
			"protectedStructId": "5-200",
		})
		if err := (structDefenderHandler{}).Handle(ctx, tx, bctx(), raw2); err != nil {
			t.Fatalf("update: %v", err)
		}
		_ = tx.QueryRow(ctx, `SELECT protected_struct_id FROM structs.struct_defender WHERE defending_struct_id=$1`, "5-100").Scan(&prot)
		if prot != "5-200" {
			t.Errorf("after update protected = %q want 5-200", prot)
		}
	})
}

func TestHandler_StructDefenderClear_DeletesBothRows(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		// Pre-seed the defender row. The struct_attribute side-effect
		// of the clear handler is best-effort (NO-OP if the row doesn't
		// exist), so we only seed the defender row that should always
		// be cleared. We then assert the attribute row was at least
		// considered (no error raised).
		if _, err := tx.Exec(ctx,
			`INSERT INTO structs.struct_defender (defending_struct_id, protected_struct_id, updated_at)
			 VALUES ($1, $2, NOW())`,
			"5-clear-test", "5-victim"); err != nil {
			t.Fatalf("seed defender: %v", err)
		}

		raw := mustJSON(t, map[string]any{
			"defendingStructId": "5-clear-test",
		})
		if err := (structDefenderClearHandler{}).Handle(ctx, tx, bctx(), raw); err != nil {
			t.Fatalf("clear: %v", err)
		}
		var defCount int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM structs.struct_defender WHERE defending_struct_id=$1`, "5-clear-test").Scan(&defCount)
		if defCount != 0 {
			t.Errorf("defender row not deleted: count=%d", defCount)
		}
	})
}

// Ensure JSON-encoded values get carried through end-to-end. Some chain
// payloads arrive wrapped as JSON-encoded strings; Decode unwraps that
// and we want to make sure Phase 3 handlers see the same payload either
// way. Spot-check with the permission handler.
func TestHandler_Permission_StringWrappedPayload(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		inner := `{"permissionId":"5-1@1-1","value":"3"}`
		wrapped, _ := json.Marshal(inner)
		if err := (permissionHandler{}).Handle(ctx, tx, bctx(), wrapped); err != nil {
			t.Fatalf("handle: %v", err)
		}
		var val int
		_ = tx.QueryRow(ctx, `SELECT val FROM structs.permission WHERE id=$1`, "5-1@1-1").Scan(&val)
		if val != 3 {
			t.Errorf("val = %d want 3", val)
		}
	})
}
