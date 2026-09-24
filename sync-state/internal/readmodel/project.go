package readmodel

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/objecttype"
)

var modelNames = []string{
	"inventory",
	"guild_bank",
	"leaderboard_player",
	"leaderboard_guild",
	"leaderboard_reactor",
	"leaderboard_provider",
	"leaderboard_substation",
	"work",
}

type inventoryMode int

const (
	inventoryFromLedger inventoryMode = iota
	inventoryFromDeltas
	inventorySkip
)

// InventoryPlan selects how Recompute writes structs.api_inventory.
type InventoryPlan struct {
	mode   inventoryMode
	deltas []buffers.LedgerDelta
}

// InventoryFromLedger rebuilds dirty address/player rows from a full
// ledger aggregate. Used by tests and as a repair path.
func InventoryFromLedger() InventoryPlan {
	return InventoryPlan{mode: inventoryFromLedger}
}

// InventoryFromDeltas applies RETURNING deltas from the ledger insert
// (replay-safe: ON CONFLICT returns nothing) then rebuilds dirty player
// rows from address inventory.
func InventoryFromDeltas(deltas []buffers.LedgerDelta) InventoryPlan {
	if deltas == nil {
		deltas = []buffers.LedgerDelta{}
	}
	return InventoryPlan{mode: inventoryFromDeltas, deltas: deltas}
}

// InventoryAlreadyWritten skips inventory (the caller ran the one-shot
// backfill in the same transaction).
func InventoryAlreadyWritten() InventoryPlan {
	return InventoryPlan{mode: inventorySkip}
}

// ValidateSchema fails before ingest starts when structs-pg has not deployed
// the complete API projection contract.
func ValidateSchema(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var missing []string
	if err := q.QueryRow(ctx, `
SELECT COALESCE(array_agg(name ORDER BY name), ARRAY[]::text[])
FROM unnest(ARRAY[
  'api_refresh_state','api_inventory','api_guild_bank',
  'api_leaderboard_player','api_leaderboard_guild',
  'api_leaderboard_reactor','api_leaderboard_provider',
  'api_leaderboard_substation','api_work'
]) AS wanted(name)
WHERE to_regclass('structs.' || name) IS NULL`).Scan(&missing); err != nil {
		return fmt.Errorf("validate api projection schema: %w", err)
	}
	if len(missing) != 0 {
		return fmt.Errorf("structs-pg API projection migrations are incomplete; missing structs.%s",
			strings.Join(missing, ", structs."))
	}
	var identCols int
	if err := q.QueryRow(ctx, `
SELECT COUNT(*) FROM information_schema.columns
 WHERE table_schema = 'structs' AND table_name = 'ledger'
   AND column_name IN ('chain_id','tx_index','msg_index','event_index')`).Scan(&identCols); err != nil {
		return fmt.Errorf("validate ledger identity columns: %w", err)
	}
	if identCols != 4 {
		return fmt.Errorf("structs-pg ledger source-event identity is incomplete; apply table-ledger-20260914-source-event-identity")
	}
	var activityPlayer bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('structs.planet_activity_player') IS NOT NULL`).Scan(&activityPlayer); err != nil {
		return fmt.Errorf("validate planet_activity_player: %w", err)
	}
	if !activityPlayer {
		return fmt.Errorf("structs-pg planet_activity_player is missing; apply the 20260915 activity-attribution sqitch changes")
	}
	var workRefresh bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'structs' AND p.proname = 'api_work_refresh'
)`).Scan(&workRefresh); err != nil {
		return fmt.Errorf("validate api_work_refresh: %w", err)
	}
	if !workRefresh {
		return fmt.Errorf("structs.api_work_refresh is missing; apply the 20260914 api_work_refresh sqitch change")
	}
	return nil
}

// Expand resolves transitive read-model dependencies after all authoritative
// handlers have run. It deliberately over-dirties rather than risk stale rows.
//
// Only players marked directly by handlers or owning a dirty address can have
// different api_inventory rows; players reached through grid objects, guild
// membership or guild-token holdings only need their leaderboard rows
// recomputed. Expand records the former set for InventoryPlayerIDs.
func Expand(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	if d == nil {
		return nil
	}
	d.inventoryPlayers = cloneSet(d.Players)
	if err := expandAddresses(ctx, tx, d); err != nil {
		return err
	}
	for id := range d.GridObjects {
		kind, _, _, err := objecttype.Parse(id)
		if err != nil {
			continue
		}
		switch kind {
		case objecttype.Player:
			d.Player(id)
		case objecttype.Substation:
			d.Substation(id)
		}
	}
	if err := expandPlayers(ctx, tx, d); err != nil {
		return err
	}
	if err := expandGuildTokenHolders(ctx, tx, d); err != nil {
		return err
	}
	return nil
}

func expandAddresses(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	ids := d.AddressIDs()
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT pa.player_id, gt.entry
FROM unnest($1::varchar[]) a(address)
LEFT JOIN structs.player_address pa ON pa.address = a.address
LEFT JOIN structs.address_tag typ
  ON typ.address = a.address AND typ.label = 'Type'
 AND typ.entry = 'Bank Collateral Pool'
LEFT JOIN structs.address_tag gt
  ON gt.address = a.address AND gt.label = 'GuildId'`, ids)
	if err != nil {
		return fmt.Errorf("expand dirty addresses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var player, guild *string
		if err := rows.Scan(&player, &guild); err != nil {
			return err
		}
		if player != nil {
			d.Player(*player)
			add(d.inventoryPlayers, *player)
		}
		if guild != nil {
			d.Guild(*guild)
		}
	}
	return rows.Err()
}

func expandPlayers(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	if len(d.Players) == 0 && len(d.Guilds) == 0 && len(d.Substations) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT id, guild_id, substation_id
FROM structs.player
WHERE id = ANY($1::varchar[])
   OR guild_id = ANY($2::varchar[])
   OR substation_id = ANY($3::varchar[])`,
		d.PlayerIDs(), d.GuildIDs(), d.SubstationIDs())
	if err != nil {
		return fmt.Errorf("expand dirty players: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var guild, substation *string
		if err := rows.Scan(&id, &guild, &substation); err != nil {
			return err
		}
		d.Player(id)
		if guild != nil {
			d.Guild(*guild)
		}
		if substation != nil {
			d.Substation(*substation)
		}
	}
	return rows.Err()
}

func expandGuildTokenHolders(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	if len(d.Guilds) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT DISTINCT owner_id
FROM structs.api_inventory
WHERE owner_type = 'player'
  AND denom = ANY(
    SELECT 'uguild.' || id FROM unnest($1::varchar[]) AS g(id)
  )`, d.GuildIDs())
	if err != nil {
		return fmt.Errorf("expand guild token holders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		d.Player(id)
	}
	return rows.Err()
}

// Recompute applies every dirty-tracked model in dependency order and
// advances each model's refresh state only after its write succeeds.
// Work is not dirty-tracked: structs.api_work_refresh diffs against
// view.work_live once per block and writes api_refresh_state for
// model='work' itself.
func Recompute(ctx context.Context, tx pgx.Tx, d *Dirty, height int64, sourceTime any, inv InventoryPlan) error {
	if err := Expand(ctx, tx, d); err != nil {
		return err
	}
	refreshed := make([]string, 0, len(modelNames))
	switch inv.mode {
	case inventorySkip:
		// Caller already wrote api_inventory (one-shot backfill).
	case inventoryFromDeltas:
		if err := applyInventoryDeltas(ctx, tx, d, inv.deltas); err != nil {
			return fmt.Errorf("inventory: %w", err)
		}
		refreshed = append(refreshed, "inventory")
	default:
		if err := recomputeInventory(ctx, tx, d); err != nil {
			return fmt.Errorf("inventory: %w", err)
		}
		refreshed = append(refreshed, "inventory")
	}
	steps := []struct {
		model string
		fn    func(context.Context, pgx.Tx, *Dirty) error
	}{
		{"guild_bank", recomputeGuildBank},
		{"leaderboard_player", recomputePlayers},
		{"leaderboard_guild", recomputeGuilds},
		{"leaderboard_reactor", recomputeReactors},
		{"leaderboard_provider", recomputeProviders},
		{"leaderboard_substation", recomputeSubstations},
	}
	for _, step := range steps {
		if err := step.fn(ctx, tx, d); err != nil {
			return fmt.Errorf("%s: %w", step.model, err)
		}
		refreshed = append(refreshed, step.model)
	}
	// Every write above shares this transaction, so one stamp after the
	// last step is equivalent to stamping each model as it finishes.
	if err := refreshModels(ctx, tx, refreshed, height, sourceTime); err != nil {
		return fmt.Errorf("refresh_state: %w", err)
	}
	if err := refreshWork(ctx, tx, height, sourceTime); err != nil {
		return err
	}
	return nil
}

func refresh(ctx context.Context, tx pgx.Tx, model string, height int64, sourceTime any) error {
	return refreshModels(ctx, tx, []string{model}, height, sourceTime)
}

func refreshModels(ctx context.Context, tx pgx.Tx, models []string, height int64, sourceTime any) error {
	if len(models) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
INSERT INTO structs.api_refresh_state(model, source_height, source_time, refreshed_at)
SELECT m, $2::bigint, $3::timestamptz, NOW() FROM unnest($1::text[]) AS m
ON CONFLICT (model) DO UPDATE
SET source_height = EXCLUDED.source_height,
    source_time = EXCLUDED.source_time,
    refreshed_at = EXCLUDED.refreshed_at`, models, height, sourceTime)
	return err
}

func refreshWork(ctx context.Context, tx pgx.Tx, height int64, sourceTime any) error {
	var inserted, updated, deleted int64
	if err := tx.QueryRow(ctx, `
SELECT inserted, updated, deleted
  FROM structs.api_work_refresh($1::bigint, $2::timestamptz)`,
		height, sourceTime).Scan(&inserted, &updated, &deleted); err != nil {
		return fmt.Errorf("work: api_work_refresh: %w", err)
	}
	return nil
}

func recomputeInventory(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	addresses, players := d.AddressIDs(), d.InventoryPlayerIDs()
	if len(addresses) == 0 && len(players) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM structs.api_inventory
WHERE (owner_type = 'address' AND owner_id = ANY($1::varchar[]))
   OR (owner_type = 'player' AND owner_id = ANY($2::varchar[]))`,
		addresses, players); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO structs.api_inventory(owner_type, owner_id, denom, balance)
SELECT 'address'::structs.object_type, l.address, l.denom,
       SUM(CASE l.direction WHEN 'credit' THEN l.amount_p ELSE -l.amount_p END)
FROM structs.ledger l
WHERE l.address = ANY($1::varchar[])
GROUP BY l.address, l.denom`, addresses); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
INSERT INTO structs.api_inventory(owner_type, owner_id, denom, balance)
SELECT 'player'::structs.object_type, pa.player_id, l.denom,
       SUM(CASE l.direction WHEN 'credit' THEN l.amount_p ELSE -l.amount_p END)
FROM structs.player_address pa
JOIN structs.ledger l ON l.address = pa.address
WHERE pa.player_id = ANY($1::varchar[])
GROUP BY pa.player_id, l.denom`, players)
	return err
}

func applyInventoryDeltas(ctx context.Context, tx pgx.Tx, d *Dirty, deltas []buffers.LedgerDelta) error {
	if len(deltas) > 0 {
		addrs := make([]string, len(deltas))
		denoms := make([]string, len(deltas))
		amounts := make([]string, len(deltas))
		for i, delta := range deltas {
			addrs[i] = delta.Address
			denoms[i] = delta.Denom
			amounts[i] = delta.Delta
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.api_inventory (owner_type, owner_id, denom, balance)
SELECT 'address'::structs.object_type, address, denom, SUM(delta)
FROM unnest($1::varchar[], $2::varchar[], $3::numeric[]) AS t(address, denom, delta)
WHERE address IS NOT NULL AND denom IS NOT NULL
GROUP BY address, denom
ON CONFLICT (owner_type, owner_id, denom)
DO UPDATE SET balance = structs.api_inventory.balance + EXCLUDED.balance`,
			addrs, denoms, amounts); err != nil {
			return err
		}
	}
	return rebuildPlayerInventory(ctx, tx, d.InventoryPlayerIDs())
}

// rebuildPlayerInventory rewrites only the player rows whose balance actually
// changed, so unchanged rows produce no dead tuples or WAL. The DELETE and the
// upsert touch disjoint rows (denoms absent vs present in fresh).
func rebuildPlayerInventory(ctx context.Context, tx pgx.Tx, players []string) error {
	if len(players) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
WITH fresh AS (
  SELECT pa.player_id, i.denom, SUM(i.balance) AS balance
  FROM structs.player_address pa
  JOIN structs.api_inventory i
    ON i.owner_type = 'address' AND i.owner_id = pa.address
  WHERE pa.player_id = ANY($1::varchar[])
  GROUP BY pa.player_id, i.denom
), stale AS (
  DELETE FROM structs.api_inventory a
  WHERE a.owner_type = 'player' AND a.owner_id = ANY($1::varchar[])
    AND NOT EXISTS (
      SELECT 1 FROM fresh f WHERE f.player_id = a.owner_id AND f.denom = a.denom
    )
)
INSERT INTO structs.api_inventory (owner_type, owner_id, denom, balance)
SELECT 'player'::structs.object_type, player_id, denom, balance FROM fresh
ON CONFLICT (owner_type, owner_id, denom) DO UPDATE
SET balance = EXCLUDED.balance
WHERE structs.api_inventory.balance IS DISTINCT FROM EXCLUDED.balance`, players)
	return err
}

func recomputeGuildBank(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	ids := d.GuildIDs()
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM structs.api_guild_bank WHERE guild_id = ANY($1::varchar[])`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
INSERT INTO structs.api_guild_bank(guild_id, denom, collateral, supply)
SELECT g.id, 'uguild.' || g.id,
       (
         SELECT SUM(CASE l.direction WHEN 'credit' THEN l.amount_p ELSE -l.amount_p END)
         FROM structs.address_tag typ
         JOIN structs.address_tag gid
           ON gid.address = typ.address AND gid.label = 'GuildId' AND gid.entry = g.id
         JOIN structs.ledger l ON l.address = typ.address
         WHERE typ.label = 'Type' AND typ.entry = 'Bank Collateral Pool'
       ),
       (
         SELECT SUM(CASE l.direction WHEN 'credit' THEN l.amount_p ELSE -l.amount_p END)
         FROM structs.ledger l
         WHERE l.action IN ('minted', 'burned') AND l.denom = 'uguild.' || g.id
       )
FROM structs.guild g
WHERE g.id = ANY($1::varchar[])`, ids)
	return err
}

// recomputePlayers upserts only changed rows (guild-token fan-out dirties
// many players whose values did not move) and deletes rows for dirty ids
// that no longer exist in structs.player.
func recomputePlayers(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	ids := d.PlayerIDs()
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM structs.api_leaderboard_player a
WHERE a.player_id = ANY($1::varchar[])
  AND NOT EXISTS (SELECT 1 FROM structs.player p WHERE p.id = a.player_id)`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
INSERT INTO structs.api_leaderboard_player AS a (player_id, username, guild_id, alpha_balance, alpha_value)
SELECT p.id, p.username, p.guild_id,
       COALESCE(SUM(i.balance) FILTER (WHERE i.denom = 'ualpha'), 0),
       CASE
         WHEN COUNT(*) FILTER (
           WHERE i.denom NOT IN ('ualpha','ualpha.infused','ualpha.defusing','ore')
             AND gb.ratio IS NULL
         ) > 0 THEN NULL
         ELSE COALESCE(SUM(CASE
           WHEN i.denom IN ('ualpha','ualpha.infused','ualpha.defusing') THEN i.balance
           WHEN i.denom = 'ore' THEN 0
           ELSE i.balance * gb.ratio
         END), 0)
       END
FROM structs.player p
LEFT JOIN structs.api_inventory i
  ON i.owner_type = 'player' AND i.owner_id = p.id
LEFT JOIN structs.api_guild_bank gb ON gb.denom = i.denom
WHERE p.id = ANY($1::varchar[])
GROUP BY p.id, p.username, p.guild_id
ON CONFLICT (player_id) DO UPDATE
SET username = EXCLUDED.username,
    guild_id = EXCLUDED.guild_id,
    alpha_balance = EXCLUDED.alpha_balance,
    alpha_value = EXCLUDED.alpha_value
WHERE (a.username, a.guild_id, a.alpha_balance, a.alpha_value)
      IS DISTINCT FROM
      (EXCLUDED.username, EXCLUDED.guild_id, EXCLUDED.alpha_balance, EXCLUDED.alpha_value)`, ids)
	return err
}

const playerMetricCTE = `
WITH player_metric AS (
  SELECT p.id, p.guild_id, p.substation_id,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'capacity'), 0) AS capacity,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'load'), 0)
           + COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'structsLoad'), 0) AS load
  FROM structs.player p
  LEFT JOIN structs.grid gr ON gr.object_id = p.id
  GROUP BY p.id, p.guild_id, p.substation_id
),
substation_metric AS (
  SELECT s.id,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'load'), 0) AS load,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'connectionCapacity'), 0) AS connection_capacity
  FROM structs.substation s
  LEFT JOIN structs.grid gr ON gr.object_id = s.id
  GROUP BY s.id
)`

func recomputeGuilds(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	ids := d.GuildIDs()
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM structs.api_leaderboard_guild WHERE guild_id = ANY($1::varchar[])`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, playerMetricCTE+`
INSERT INTO structs.api_leaderboard_guild(
  guild_id, name, onchain_name, player_count, collateral, supply,
  member_capacity, member_load, shared_connection_capacity
)
SELECT g.id, gm.name, g.name, COUNT(pm.id), gb.collateral, gb.supply,
       COALESCE(SUM(pm.capacity), 0), COALESCE(SUM(pm.load), 0),
       COALESCE((
         SELECT SUM(sm.connection_capacity)
         FROM substation_metric sm
         WHERE sm.id IN (
           SELECT DISTINCT pm2.substation_id FROM player_metric pm2
           WHERE pm2.guild_id = g.id AND pm2.substation_id IS NOT NULL
         )
       ), 0)
FROM structs.guild g
LEFT JOIN structs.guild_meta gm ON gm.id = g.id
LEFT JOIN player_metric pm ON pm.guild_id = g.id
LEFT JOIN structs.api_guild_bank gb ON gb.guild_id = g.id AND gb.denom = 'uguild.' || g.id
WHERE g.id = ANY($1::varchar[])
GROUP BY g.id, gm.name, g.name, gb.collateral, gb.supply`, ids)
	return err
}

func recomputeReactors(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	ids := d.ReactorIDs()
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM structs.api_leaderboard_reactor WHERE reactor_id = ANY($1::varchar[])`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
INSERT INTO structs.api_leaderboard_reactor(reactor_id, fuel, power)
SELECT r.id, COALESCE(SUM(i.fuel_p), 0), COALESCE(SUM(i.power_p), 0)
FROM structs.reactor r
LEFT JOIN structs.infusion i
  ON i.destination_id = r.id AND i.destination_type = 'reactor'
WHERE r.id = ANY($1::varchar[])
GROUP BY r.id`, ids)
	return err
}

func recomputeProviders(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	ids := d.ProviderIDs()
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM structs.api_leaderboard_provider WHERE provider_id = ANY($1::varchar[])`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
INSERT INTO structs.api_leaderboard_provider(provider_id, owner, rate_amount, rate_denom, agreement_count)
SELECT p.id, p.owner, p.rate_amount, p.rate_denom, COUNT(a.id)
FROM structs.provider p
LEFT JOIN structs.agreement a ON a.provider_id = p.id
WHERE p.id = ANY($1::varchar[])
GROUP BY p.id, p.owner, p.rate_amount, p.rate_denom`, ids)
	return err
}

func recomputeSubstations(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	ids := d.SubstationIDs()
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM structs.api_leaderboard_substation WHERE substation_id = ANY($1::varchar[])`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, playerMetricCTE+`
INSERT INTO structs.api_leaderboard_substation(
  substation_id, owner, load, member_capacity,
  shared_connection_capacity, player_count
)
SELECT s.id, s.owner, sm.load, COALESCE(SUM(pm.capacity), 0),
       sm.connection_capacity, COUNT(pm.id)
FROM structs.substation s
JOIN substation_metric sm ON sm.id = s.id
LEFT JOIN player_metric pm ON pm.substation_id = s.id
WHERE s.id = ANY($1::varchar[])
GROUP BY s.id, s.owner, sm.load, sm.connection_capacity`, ids)
	return err
}
