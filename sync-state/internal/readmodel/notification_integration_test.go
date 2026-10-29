package readmodel

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Like the lock-wait fixture, this uses real commits in the disposable Sqitch
// database. A rolled-back transaction cannot test PostgreSQL NOTIFY delivery.
func TestEconomicOwnershipNotificationsAfterCommit(t *testing.T) {
	if os.Getenv("INTEGRATION_MAINTENANCE_TESTS") != "1" {
		t.Skip("INTEGRATION_MAINTENANCE_TESTS=1 requires a disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	writer, err := pgx.Connect(ctx, os.Getenv("INTEGRATION_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	listener, err := pgx.Connect(ctx, os.Getenv("INTEGRATION_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(context.Background())
	const p1, p2, guild, chain = "1-993301", "1-993302", "0-993301", "ownership-notify-test"
	const v1, v2, vg, retained = "notify-vault-one", "notify-vault-two", "notify-vault-guild", "notify-retained"
	cleanup := func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		tx, err := writer.Begin(cleanupCtx)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(cleanupCtx)
		for _, stmt := range []struct {
			sql string
			arg any
		}{
			{`DELETE FROM structs.ledger WHERE chain_id=$1`, chain},
			{`DELETE FROM structs.infusion WHERE destination_id=$1`, "3-993301"},
			{`DELETE FROM structs.vault WHERE owner_id=ANY($1::varchar[])`, []string{p1, p2, guild}},
			{`DELETE FROM structs.player_address WHERE player_id=ANY($1::varchar[])`, []string{p1, p2}},
			{`DELETE FROM structs.grid WHERE object_id=ANY($1::varchar[])`, []string{p1, p2}},
			{`DELETE FROM structs.player WHERE id=ANY($1::varchar[])`, []string{p1, p2}},
			{`DELETE FROM structs.player_internal_pending WHERE guild_id=$1`, guild},
			{`DELETE FROM signer.role WHERE guild_id=$1`, guild},
			{`DELETE FROM structs.guild WHERE id=$1`, guild},
			{`DELETE FROM structs.api_work_dirty WHERE id=ANY($1::varchar[])`, []string{p1, p2, guild}},
		} {
			if _, err := tx.Exec(cleanupCtx, stmt.sql, stmt.arg); err != nil {
				t.Error(err)
				return
			}
		}
		if err := tx.Commit(cleanupCtx); err != nil {
			t.Error(err)
		}
	}
	defer cleanup()
	if _, err := listener.Exec(ctx, `LISTEN grass`); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	seedIsolatedGuild(t, ctx, tx, guild, "notification-guild")
	seedIsolatedPlayer(t, ctx, tx, p1, "notification-player-one", guild, "")
	seedIsolatedPlayer(t, ctx, tx, p2, "notification-player-two", guild, "")
	if _, err := tx.Exec(ctx, `INSERT INTO structs.vault(owner_id,address,account_number) VALUES($1,$2,0),($3,$4,0),($5,$6,0)`, p1, v1, p2, v2, guild, vg); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.player_address(address,player_id,status,guild_id) VALUES($1,$2,'approved',$3)`, retained, p1, guild); err != nil {
		t.Fatal(err)
	}
	for i, leg := range []struct{ address, counterparty, amount, action, direction string }{
		{v1, v2, "18446744073709551615", "sent", "debit"},
		{vg, v1, "5", "received", "credit"},
		{retained, "external", "4", "received", "credit"},
		{"external", strings.Repeat("界", 3000), "3", "received", "credit"},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO structs.ledger(address,counterparty,amount_p,block_height,time,action,direction,denom,chain_id,tx_index,msg_index,event_index)
VALUES($1,$2,$3::numeric,101,TIMESTAMPTZ '2026-01-01Z',$4,$5,'ualpha',$6,0,0,$7)`, leg.address, leg.counterparty, leg.amount, leg.action, leg.direction, chain, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.infusion(destination_id,address,player_id,owner_id,fuel_p,defusing_p,power_p,ratio_p)
VALUES('3-993301',$1,$2,$2,10,0,5,1)`, retained, p1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE structs.player_address SET player_id=$1 WHERE address=$2`, p2, retained); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE structs.infusion SET owner_id=$1 WHERE address=$2`, p2, retained); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM structs.infusion WHERE address=$1`, retained); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.infusion(destination_id,address,player_id,owner_id,fuel_p,defusing_p,power_p,ratio_p)
VALUES('3-993301',$1,$2,$3,10,0,5,1)`, vg, p1, guild); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM structs.infusion WHERE address=$1`, vg); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var ledger []map[string]json.RawMessage
	infusion := map[string]int{}
	decodeString := func(m map[string]json.RawMessage, key string) string {
		var s string
		_ = json.Unmarshal(m[key], &s)
		return s
	}
	for len(ledger) < 4 || len(infusion) < 6 {
		n, err := listener.WaitForNotification(ctx)
		if err != nil {
			t.Fatalf("notification delivery incomplete (ledger=%d infusion=%v): %v", len(ledger), infusion, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(n.Payload), &m); err != nil {
			t.Fatal(err)
		}
		if decodeString(m, "chain_id") == chain {
			if len(n.Payload) > 7995 {
				t.Fatalf("oversized inventory notification: %d bytes", len(n.Payload))
			}
			ledger = append(ledger, m)
		}
		if decodeString(m, "destination_id") == "3-993301" && decodeString(m, "invalidates") == "infusion" {
			key := decodeString(m, "notification_owner_id") + "/" + decodeString(m, "operation") + "/" + decodeString(m, "address")
			infusion[key]++
			kind := "player"
			if decodeString(m, "notification_owner_id") == guild {
				kind = "guild"
			}
			if decodeString(m, "subject") != "structs.grid."+kind+"."+decodeString(m, "notification_owner_id") {
				t.Fatalf("wrong infusion subject: %s", n.Payload)
			}
		}
	}
	if decodeString(ledger[0], "owner_id") != p1 || decodeString(ledger[0], "counterparty_player_id") != p2 || decodeString(ledger[0], "amount_p_exact") != "18446744073709551615" {
		t.Fatalf("vault transfer attribution/precision: %s", ledger[0])
	}
	if decodeString(ledger[1], "player_id") != "noPlayer" || decodeString(ledger[1], "owner_id") != guild {
		t.Fatalf("guild vault attributed to a player: %s", ledger[1])
	}
	if decodeString(ledger[2], "owner_id") != p1 || string(ledger[2]["is_vault"]) != "false" {
		t.Fatalf("retained address attribution: %s", ledger[2])
	}
	if string(ledger[3]["stub"]) != "true" || decodeString(ledger[3], "id_exact") == "" {
		t.Fatalf("byte-limit fallback lost exact row identity: %s", ledger[3])
	}
	for _, key := range []string{p1 + "/insert/" + retained, p1 + "/update/" + retained, p2 + "/update/" + retained, p2 + "/delete/" + retained, guild + "/insert/" + vg, guild + "/delete/" + vg} {
		if infusion[key] != 1 {
			t.Fatalf("missing/duplicate invalidation %s: %v", key, infusion)
		}
	}
}
