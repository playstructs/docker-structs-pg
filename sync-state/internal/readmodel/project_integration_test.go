package readmodel

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func integrationTx(t *testing.T, fn func(context.Context, pgx.Tx)) {
	t.Helper()
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	fn(ctx, tx)
}

func TestRecomputeAgainstDeployedSchema(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		d := NewDirty()
		for _, spec := range []struct {
			query string
			add   func(string)
		}{
			{`SELECT address FROM structs.ledger LIMIT 1`, d.Address},
			{`SELECT id FROM structs.player LIMIT 1`, d.Player},
			{`SELECT id FROM structs.guild LIMIT 1`, d.Guild},
			{`SELECT id FROM structs.reactor LIMIT 1`, d.Reactor},
			{`SELECT id FROM structs.provider LIMIT 1`, d.Provider},
			{`SELECT id FROM structs.substation LIMIT 1`, d.Substation},
		} {
			var id string
			if err := tx.QueryRow(ctx, spec.query).Scan(&id); err == nil {
				spec.add(id)
			} else if err != pgx.ErrNoRows {
				t.Fatal(err)
			}
		}
		if err := Recompute(ctx, tx, d, 999999999, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var models int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM structs.api_refresh_state WHERE source_height=999999999`,
		).Scan(&models); err != nil {
			t.Fatal(err)
		}
		if models != len(modelNames) {
			t.Fatalf("refreshed models=%d want %d", models, len(modelNames))
		}
	})
}

func TestInventoryRecomputeIsReplaySafeAndCombinesAddresses(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		var player string
		err := tx.QueryRow(ctx, `
SELECT player_id FROM structs.player_address
GROUP BY player_id HAVING COUNT(*) > 1 LIMIT 1`).Scan(&player)
		if err == pgx.ErrNoRows {
			t.Skip("fixture has no player with multiple addresses")
		}
		if err != nil {
			t.Fatal(err)
		}
		d := NewDirty()
		d.Player(player)
		if err := recomputeInventory(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var first string
		if err := tx.QueryRow(ctx, `
SELECT balance::text FROM structs.api_inventory
WHERE owner_type='player' AND owner_id=$1 AND denom='ualpha'`, player).Scan(&first); err != nil {
			t.Fatal(err)
		}
		if err := recomputeInventory(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var second string
		if err := tx.QueryRow(ctx, `
SELECT balance::text FROM structs.api_inventory
WHERE owner_type='player' AND owner_id=$1 AND denom='ualpha'`, player).Scan(&second); err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Fatalf("replay changed balance: first=%s second=%s", first, second)
		}
	})
}

func TestSharedSubstationCapacityStoredOnce(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		var id string
		err := tx.QueryRow(ctx, `
SELECT substation_id FROM structs.player
WHERE substation_id IS NOT NULL AND substation_id <> ''
GROUP BY substation_id HAVING COUNT(*) > 1 LIMIT 1`).Scan(&id)
		if err == pgx.ErrNoRows {
			t.Skip("fixture has no shared substation")
		}
		if err != nil {
			t.Fatal(err)
		}
		d := NewDirty()
		d.Substation(id)
		if err := Expand(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		if err := recomputeSubstations(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var got, want string
		if err := tx.QueryRow(ctx, `
SELECT shared_connection_capacity::text
FROM structs.api_leaderboard_substation WHERE substation_id=$1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(val) FILTER (WHERE attribute_type='connectionCapacity'),0)::text
FROM structs.grid WHERE object_id=$1`, id).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("shared capacity duplicated: got=%s want=%s", got, want)
		}
	})
}

func TestProviderCurrentRateWithExistingAgreements(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		var id string
		err := tx.QueryRow(ctx, `
SELECT p.id FROM structs.provider p
JOIN structs.agreement a ON a.provider_id=p.id LIMIT 1`).Scan(&id)
		if err == pgx.ErrNoRows {
			t.Skip("fixture has no provider with agreements")
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE structs.provider SET rate_amount=123456789, rate_denom='utest' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		d := NewDirty()
		d.Provider(id)
		if err := recomputeProviders(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var amount, denom string
		var agreements int64
		if err := tx.QueryRow(ctx, `
SELECT rate_amount::text, rate_denom, agreement_count
FROM structs.api_leaderboard_provider WHERE provider_id=$1`, id).
			Scan(&amount, &denom, &agreements); err != nil {
			t.Fatal(err)
		}
		if amount != "123456789" || denom != "utest" || agreements == 0 {
			t.Fatalf("provider projection amount=%s denom=%s agreements=%d", amount, denom, agreements)
		}
	})
}

func TestGuildBankMultiplePoolsAndUnavailableInputs(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		var guild, a1, a2 string
		if err := tx.QueryRow(ctx, `SELECT id FROM structs.guild LIMIT 1`).Scan(&guild); err != nil {
			if err == pgx.ErrNoRows {
				t.Skip("fixture has no guild")
			}
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, `
SELECT DISTINCT l.address FROM structs.ledger l
WHERE NOT EXISTS (
  SELECT 1 FROM structs.address_tag t
  WHERE t.address=l.address AND t.label='Type' AND t.entry='Bank Collateral Pool'
) LIMIT 2`)
		if err != nil {
			t.Fatal(err)
		}
		var addresses []string
		for rows.Next() {
			var address string
			if err := rows.Scan(&address); err != nil {
				t.Fatal(err)
			}
			addresses = append(addresses, address)
		}
		rows.Close()
		if len(addresses) < 2 {
			t.Skip("fixture has fewer than two untagged ledger addresses")
		}
		a1, a2 = addresses[0], addresses[1]
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.address_tag(address,label,entry,created_at,updated_at) VALUES
  ($1,'Type','Bank Collateral Pool',NOW(),NOW()),($1,'GuildId',$3,NOW(),NOW()),
  ($2,'Type','Bank Collateral Pool',NOW(),NOW()),($2,'GuildId',$3,NOW(),NOW())
ON CONFLICT(address,label) DO UPDATE SET entry=EXCLUDED.entry`,
			a1, a2, guild); err != nil {
			t.Fatal(err)
		}
		d := NewDirty()
		d.Guild(guild)
		if err := recomputeGuildBank(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var got, want *string
		if err := tx.QueryRow(ctx, `
SELECT collateral::text FROM structs.api_guild_bank
WHERE guild_id=$1 AND denom='uguild.' || $1`, guild).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `
SELECT SUM(CASE l.direction WHEN 'credit' THEN l.amount_p ELSE -l.amount_p END)::text
FROM structs.address_tag typ
JOIN structs.address_tag gid
  ON gid.address=typ.address AND gid.label='GuildId' AND gid.entry=$1
JOIN structs.ledger l ON l.address=typ.address
WHERE typ.label='Type' AND typ.entry='Bank Collateral Pool'`, guild).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got == nil || want == nil || *got != *want {
			t.Fatalf("multi-pool collateral got=%v want=%v", got, want)
		}

		if _, err := tx.Exec(ctx, `
DELETE FROM structs.ledger
WHERE denom='uguild.' || $1 AND action IN ('minted','burned')`, guild); err != nil {
			t.Fatal(err)
		}
		if err := recomputeGuildBank(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var supply, ratio *string
		if err := tx.QueryRow(ctx, `
SELECT supply::text, ratio::text FROM structs.api_guild_bank
WHERE guild_id=$1 AND denom='uguild.' || $1`, guild).Scan(&supply, &ratio); err != nil {
			t.Fatal(err)
		}
		if supply != nil || ratio != nil {
			t.Fatalf("unavailable supply should produce NULLs: supply=%v ratio=%v", supply, ratio)
		}
	})
}

func seedIsolatedPlayer(t *testing.T, ctx context.Context, tx pgx.Tx, id, username, guildID, substationID string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
INSERT INTO structs.player (id, index, username, guild_id, substation_id, created_at, updated_at)
VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), NOW(), NOW())
ON CONFLICT (id) DO UPDATE
SET username = EXCLUDED.username,
    guild_id = EXCLUDED.guild_id,
    substation_id = EXCLUDED.substation_id`,
		id, 0, username, guildID, substationID); err != nil {
		t.Fatalf("seed player %s: %v", id, err)
	}
}

func seedIsolatedGuild(t *testing.T, ctx context.Context, tx pgx.Tx, id, name string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
INSERT INTO structs.guild (id, index, name, created_at, updated_at)
VALUES ($1, 0, $2, NOW(), NOW())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`, id, name); err != nil {
		t.Fatalf("seed guild %s: %v", id, err)
	}
}

func seedIsolatedLedger(t *testing.T, ctx context.Context, tx pgx.Tx, address, amount, action, direction, denom string, height int64) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
INSERT INTO structs.ledger (address, amount_p, block_height, time, action, direction, denom)
VALUES ($1, $2::numeric, $3, TIMESTAMPTZ '2026-01-01 00:00:00+00', $4, $5, $6)`,
		address, amount, height, action, direction, denom); err != nil {
		t.Fatalf("seed ledger %s: %v", address, err)
	}
}

func TestIsolatedInventoryMultiAddressAndLateLedger(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		player := "1-991110"
		a1, a2 := "structs1isoinv1", "structs1isoinv2"
		seedIsolatedPlayer(t, ctx, tx, player, "iso-inv", "", "")
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.player_address (address, player_id, status, created_at, updated_at) VALUES
  ($1, $3, 'approved', NOW(), NOW()),
  ($2, $3, 'approved', NOW(), NOW())
ON CONFLICT (address) DO UPDATE SET player_id = EXCLUDED.player_id`, a1, a2, player); err != nil {
			t.Fatal(err)
		}
		seedIsolatedLedger(t, ctx, tx, a1, "100", "received", "credit", "ualpha", 10)
		seedIsolatedLedger(t, ctx, tx, a2, "40", "sent", "debit", "ualpha", 11)
		d := NewDirty()
		d.Player(player)
		d.Address(a1)
		d.Address(a2)
		if err := recomputeInventory(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var playerBal, a1Bal, a2Bal string
		if err := tx.QueryRow(ctx, `SELECT balance::text FROM structs.api_inventory WHERE owner_type='player' AND owner_id=$1 AND denom='ualpha'`, player).Scan(&playerBal); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT balance::text FROM structs.api_inventory WHERE owner_type='address' AND owner_id=$1 AND denom='ualpha'`, a1).Scan(&a1Bal); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT balance::text FROM structs.api_inventory WHERE owner_type='address' AND owner_id=$1 AND denom='ualpha'`, a2).Scan(&a2Bal); err != nil {
			t.Fatal(err)
		}
		if playerBal != "60" || a1Bal != "100" || a2Bal != "-40" {
			t.Fatalf("balances player=%s a1=%s a2=%s", playerBal, a1Bal, a2Bal)
		}

		seedIsolatedLedger(t, ctx, tx, a1, "15", "received", "credit", "ualpha", 5)
		if err := recomputeInventory(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT balance::text FROM structs.api_inventory WHERE owner_type='player' AND owner_id=$1 AND denom='ualpha'`, player).Scan(&playerBal); err != nil {
			t.Fatal(err)
		}
		if playerBal != "75" {
			t.Fatalf("late ledger did not recompute player balance: %s", playerBal)
		}
	})
}

func TestIsolatedGuildBankZeroSupplyAndOrphanDelete(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		guild := "0-991120"
		seedIsolatedGuild(t, ctx, tx, guild, "iso-bank")
		d := NewDirty()
		d.Guild(guild)
		if err := recomputeGuildBank(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var supply, ratio, collateral *string
		if err := tx.QueryRow(ctx, `
SELECT supply::text, ratio::text, collateral::text
FROM structs.api_guild_bank WHERE guild_id=$1`, guild).Scan(&supply, &ratio, &collateral); err != nil {
			t.Fatal(err)
		}
		if supply != nil || ratio != nil || collateral != nil {
			t.Fatalf("empty bank should be NULL: supply=%v ratio=%v collateral=%v", supply, ratio, collateral)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM structs.guild WHERE id=$1`, guild); err != nil {
			t.Fatal(err)
		}
		if err := recomputeGuildBank(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM structs.api_guild_bank WHERE guild_id=$1`, guild).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("deleted guild left %d bank rows", n)
		}
	})
}

func TestIsolatedSharedSubstationAndReactorProvider(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		sub := "4-991130"
		p1, p2 := "1-991131", "1-991132"
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.substation (id, owner, created_at, updated_at)
VALUES ($1, $2, NOW(), NOW())
ON CONFLICT (id) DO UPDATE SET owner = EXCLUDED.owner`, sub, p1); err != nil {
			t.Fatal(err)
		}
		seedIsolatedPlayer(t, ctx, tx, p1, "iso-s1", "", sub)
		seedIsolatedPlayer(t, ctx, tx, p2, "iso-s2", "", sub)
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.grid (id, attribute_type, object_type, object_index, object_id, val, updated_at) VALUES
  ('2-1-991131', 'capacity', 'player', 991131, $1, 10, NOW()),
  ('3-1-991131', 'load', 'player', 991131, $1, 4, NOW()),
  ('2-1-991132', 'capacity', 'player', 991132, $2, 20, NOW()),
  ('3-1-991132', 'load', 'player', 991132, $2, 6, NOW()),
  ('3-4-991130', 'load', 'substation', 991130, $3, 99, NOW()),
  ('6-4-991130', 'connectionCapacity', 'substation', 991130, $3, 500, NOW())
ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`, p1, p2, sub); err != nil {
			t.Fatal(err)
		}
		d := NewDirty()
		d.GridObject(p1)
		d.GridObject(p2)
		d.GridObject(sub)
		if err := Expand(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		if _, ok := d.Players[p1]; !ok {
			t.Fatalf("expand missed player %s: %v", p1, d.Players)
		}
		if _, ok := d.Substations[sub]; !ok {
			t.Fatalf("expand missed substation %s: %v", sub, d.Substations)
		}
		if err := recomputeSubstations(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var load, memberCap, shared string
		var players int64
		if err := tx.QueryRow(ctx, `
SELECT load::text, member_capacity::text, shared_connection_capacity::text, player_count
FROM structs.api_leaderboard_substation WHERE substation_id=$1`, sub).
			Scan(&load, &memberCap, &shared, &players); err != nil {
			t.Fatal(err)
		}
		if load != "99" || memberCap != "30" || shared != "500" || players != 2 {
			t.Fatalf("substation load=%s member=%s shared=%s players=%d", load, memberCap, shared, players)
		}

		reactor := "3-991140"
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.reactor (id, created_at, updated_at) VALUES ($1, NOW(), NOW())
ON CONFLICT (id) DO NOTHING`, reactor); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.infusion (destination_id, address, destination_type, fuel_p, power_p, created_at, updated_at)
VALUES ($1, 'structs1isoinf', 'reactor', 12, 34, NOW(), NOW())
ON CONFLICT (destination_id, address) DO UPDATE
SET fuel_p = EXCLUDED.fuel_p, power_p = EXCLUDED.power_p`, reactor); err != nil {
			t.Fatal(err)
		}
		d.Reactor(reactor)
		if err := recomputeReactors(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var fuel, power string
		if err := tx.QueryRow(ctx, `SELECT fuel::text, power::text FROM structs.api_leaderboard_reactor WHERE reactor_id=$1`, reactor).Scan(&fuel, &power); err != nil {
			t.Fatal(err)
		}
		if fuel != "12" || power != "34" {
			t.Fatalf("reactor fuel=%s power=%s", fuel, power)
		}

		provider := "10-991150"
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.provider (id, owner, rate_amount, rate_denom, created_at, updated_at)
VALUES ($1, 'owner', 7, 'ualpha', NOW(), NOW())
ON CONFLICT (id) DO UPDATE SET rate_amount = EXCLUDED.rate_amount, rate_denom = EXCLUDED.rate_denom`, provider); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.agreement (id, provider_id, capacity, start_block, end_block, created_at, updated_at)
VALUES ('11-991151', $1, 1, 1, 2, NOW(), NOW())
ON CONFLICT (id) DO NOTHING`, provider); err != nil {
			t.Fatal(err)
		}
		d.Provider(provider)
		if err := recomputeProviders(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var rate, denom string
		var agreements int64
		if err := tx.QueryRow(ctx, `
SELECT rate_amount::text, rate_denom, agreement_count
FROM structs.api_leaderboard_provider WHERE provider_id=$1`, provider).
			Scan(&rate, &denom, &agreements); err != nil {
			t.Fatal(err)
		}
		if rate != "7" || denom != "ualpha" || agreements != 1 {
			t.Fatalf("provider rate=%s denom=%s agreements=%d", rate, denom, agreements)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM structs.reactor WHERE id=$1`, reactor); err != nil {
			t.Fatal(err)
		}
		if err := recomputeReactors(ctx, tx, d); err != nil {
			t.Fatal(err)
		}
		var left int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM structs.api_leaderboard_reactor WHERE reactor_id=$1`, reactor).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Fatalf("deleted reactor left %d leaderboard rows", left)
		}
	})
}
