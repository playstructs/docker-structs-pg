package readmodel

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/objecttype"
	"sync-state/internal/timing"
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
// bank ledger aggregate and current synthetic claims. Used as a repair path.
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
	// Push the bound address set into the UNION view even with a generic plan;
	// otherwise a single native publication can scan every known owner.
	rows, err := tx.Query(ctx, `
SELECT ao.owner_type::text, ao.owner_id, gt.entry
FROM unnest($1::varchar[]) a(address)
LEFT JOIN (
    SELECT * FROM structs.asset_owner WHERE address = ANY($1::varchar[])
) ao ON ao.address = a.address
LEFT JOIN structs.address_tag typ
  ON typ.address = a.address AND typ.label = 'Type'
 AND typ.entry = 'Bank Collateral Pool'
LEFT JOIN structs.address_tag gt
  ON gt.address = a.address AND gt.label = 'GuildId' AND typ.address IS NOT NULL`, ids)
	if err != nil {
		return fmt.Errorf("expand dirty addresses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ownerType, owner, guild *string
		if err := rows.Scan(&ownerType, &owner, &guild); err != nil {
			return err
		}
		if owner != nil && ownerType != nil {
			if *ownerType == "guild" {
				d.Guild(*owner)
			} else {
				d.Player(*owner)
				add(d.inventoryPlayers, *owner)
			}
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
	return recompute(ctx, tx, d, height, sourceTime, inv, false)
}

func recompute(ctx context.Context, tx pgx.Tx, d *Dirty, height int64, sourceTime any, inv InventoryPlan, fullWork bool) error {
	next := timing.Phases(ctx)
	defer next("")
	next("projection.expand")
	if err := Expand(ctx, tx, d); err != nil {
		return err
	}
	refreshed := make([]string, 0, len(modelNames))
	timing.Count(ctx, "projection_players", int64(len(d.Players)))
	timing.Count(ctx, "projection_guilds", int64(len(d.Guilds)))
	timing.Count(ctx, "projection_substations", int64(len(d.Substations)))
	next("projection.inventory")
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
		next("projection." + step.model)
		if err := step.fn(ctx, tx, d); err != nil {
			return fmt.Errorf("%s: %w", step.model, err)
		}
		refreshed = append(refreshed, step.model)
	}
	// Every write above shares this transaction, so one stamp after the
	// last step is equivalent to stamping each model as it finishes.
	next("projection.watermarks")
	if err := refreshModels(ctx, tx, refreshed, height, sourceTime); err != nil {
		return fmt.Errorf("refresh_state: %w", err)
	}
	next("projection.work")
	if err := refreshWork(ctx, tx, height, sourceTime, fullWork); err != nil {
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
INSERT INTO structs.api_refresh_state(model, source_height, source_time, refreshed_at, inventory_authority)
SELECT m, $2::bigint, $3::timestamptz, NOW(),
       CASE WHEN m='inventory' THEN 'current-claims-v1' ELSE 'ledger' END
FROM unnest($1::text[]) AS m
ON CONFLICT (model) DO UPDATE
SET source_height = EXCLUDED.source_height,
    source_time = EXCLUDED.source_time,
    refreshed_at = EXCLUDED.refreshed_at,
    inventory_authority = EXCLUDED.inventory_authority`, models, height, sourceTime)
	return err
}

func refreshWork(ctx context.Context, tx pgx.Tx, height int64, sourceTime any, full bool) error {
	var inserted, updated, deleted int64
	query := `
SELECT inserted, updated, deleted
  FROM structs.api_work_refresh($1::bigint, $2::timestamptz)`
	if full {
		query = `SELECT inserted, updated, deleted
  FROM structs.api_work_refresh_full($1::bigint, $2::timestamptz, FALSE)`
	}
	if err := tx.QueryRow(ctx, query,
		height, sourceTime).Scan(&inserted, &updated, &deleted); err != nil {
		return fmt.Errorf("work: api_work_refresh: %w", err)
	}
	timing.Count(ctx, "work_inserted", inserted)
	timing.Count(ctx, "work_updated", updated)
	timing.Count(ctx, "work_deleted", deleted)
	return nil
}

func recomputeInventory(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	addresses, err := inventoryAddresses(ctx, tx, d)
	if err != nil {
		return err
	}
	if len(addresses) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM structs.api_inventory
WHERE owner_type='address' AND owner_id=ANY($1::varchar[])`, addresses); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.api_inventory(owner_type,owner_id,denom,balance)
SELECT 'address'::structs.object_type,address,denom,balance
FROM structs.api_inventory_address_expected($1::varchar[],TRUE)`, addresses); err != nil {
			return err
		}
	}
	return rebuildOwnerInventory(ctx, tx, d.InventoryOwnerIDs())
}

// Include current owner addresses even when only an ownership/player event
// dirtied the owner. A missing native snapshot must materialize as unknown.
func inventoryAddresses(ctx context.Context, tx pgx.Tx, d *Dirty) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT address FROM unnest($1::varchar[]) a(address)
UNION SELECT address FROM structs.asset_owner WHERE owner_id=ANY($2::varchar[])`,
		d.AddressIDs(), d.InventoryOwnerIDs())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func refreshInventoryClaims(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	addresses, err := inventoryAddresses(ctx, tx, d)
	if err != nil || len(addresses) == 0 {
		return err
	}
	_, err = tx.Exec(ctx, `
WITH candidates AS (
  SELECT address FROM unnest($1::varchar[]) a(address)
  WHERE EXISTS (SELECT 1 FROM structs.asset_owner o WHERE o.address=a.address)
     OR EXISTS (SELECT 1 FROM sync_state.native_staking_snapshot s
                JOIN sync_state.sync_cursor c ON c.chain_id=s.chain_id WHERE s.address=a.address)
     OR EXISTS (SELECT 1 FROM structs.infusion i WHERE i.address=a.address)
     OR EXISTS (SELECT 1 FROM structs.api_inventory i WHERE i.owner_type='address'
                AND i.owner_id=a.address AND i.denom IN ('ualpha.infused','ualpha.defusing'))
), fresh AS MATERIALIZED (
  SELECT * FROM structs.api_inventory_current_claims(ARRAY(SELECT address FROM candidates))
), stale AS (
  DELETE FROM structs.api_inventory i
  WHERE i.owner_type='address' AND i.owner_id=ANY($1::varchar[])
    AND i.denom IN ('ualpha.infused','ualpha.defusing')
    AND NOT EXISTS (SELECT 1 FROM fresh f WHERE f.address=i.owner_id AND f.denom=i.denom)
)
INSERT INTO structs.api_inventory(owner_type,owner_id,denom,balance)
SELECT 'address'::structs.object_type,address,denom,balance FROM fresh
ON CONFLICT (owner_type,owner_id,denom) DO UPDATE SET balance=EXCLUDED.balance
WHERE structs.api_inventory.balance IS DISTINCT FROM EXCLUDED.balance`, addresses)
	return err
}

// RefreshNativeClaims publishes dependent inventory/value in the same locked
// transaction as native evidence. It never advances a projection watermark:
// deferred or incomplete models still require the normal canonical rebuild.
func RefreshNativeClaims(ctx context.Context, tx pgx.Tx, address string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM structs.api_refresh_state
WHERE model='inventory' AND inventory_authority='current-claims-v1')`).Scan(&active); err != nil || !active {
		return err
	}
	d := NewDirty()
	d.Address(address)
	// No guild bank ratio or game capacity changed. Avoid the general
	// expansion's guild-member/token-holder fan-out on each LCD result.
	d.inventoryPlayers = cloneSet(d.Players)
	if err := expandAddresses(ctx, tx, d); err != nil {
		return err
	}
	if err := applyInventoryDeltas(ctx, tx, d, nil); err != nil {
		return err
	}
	return recomputePlayers(ctx, tx, d)
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
  AND denom NOT IN ('ualpha.infused','ualpha.defusing')
GROUP BY address, denom
ON CONFLICT (owner_type, owner_id, denom)
DO UPDATE SET balance = structs.api_inventory.balance + EXCLUDED.balance`,
			addrs, denoms, amounts); err != nil {
			return err
		}
	}
	if err := refreshInventoryClaims(ctx, tx, d); err != nil {
		return err
	}
	return rebuildOwnerInventory(ctx, tx, d.InventoryOwnerIDs())
}

// rebuildOwnerInventory uses current economic ownership for both players and
// guild treasuries. Removed/revoked associations remove only attribution, never
// the unowned address balance or historical receipts.
func rebuildOwnerInventory(ctx context.Context, tx pgx.Tx, owners []string) error {
	if len(owners) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
WITH fresh AS (
  SELECT ao.owner_type, ao.owner_id, i.denom, CASE WHEN count(*)=count(i.balance) THEN SUM(i.balance) END AS balance
  FROM structs.asset_owner ao
  JOIN structs.api_inventory i ON i.owner_type='address' AND i.owner_id=ao.address
  WHERE ao.owner_id=ANY($1::varchar[])
  GROUP BY ao.owner_type, ao.owner_id, i.denom
), stale AS (
  DELETE FROM structs.api_inventory a
  WHERE a.owner_type IN ('player','guild') AND a.owner_id=ANY($1::varchar[])
    AND NOT EXISTS (SELECT 1 FROM fresh f
      WHERE f.owner_type=a.owner_type AND f.owner_id=a.owner_id AND f.denom=a.denom)
)
INSERT INTO structs.api_inventory(owner_type,owner_id,denom,balance)
SELECT owner_type,owner_id,denom,balance FROM fresh
ON CONFLICT (owner_type,owner_id,denom) DO UPDATE SET balance=EXCLUDED.balance
WHERE structs.api_inventory.balance IS DISTINCT FROM EXCLUDED.balance`, owners)
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
         WHERE typ.label = 'Type' AND typ.entry = 'Bank Collateral Pool' AND l.denom = 'ualpha'
       ),
       (
         SELECT SUM(CASE l.direction WHEN 'credit' THEN l.amount_p ELSE -l.amount_p END)
         FROM structs.ledger l
         WHERE l.action IN ('genesis', 'minted', 'burned') AND l.denom = 'uguild.' || g.id
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
           WHERE i.denom IS NOT NULL AND (i.balance IS NULL
             OR (i.denom NOT IN ('ualpha','ualpha.infused','ualpha.defusing','ore')
                 AND gb.ratio IS NULL))
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

// Restrict players before aggregating grid rows, including every member of each
// requested group. Guilds can share substations, so their station metric must
// remain independent of guild membership. Substation refreshes restrict both.
var guildMetricCTE = fmt.Sprintf(playerMetricSQL, "WHERE p.guild_id = ANY($1::varchar[])", "")
var substationMetricCTE = fmt.Sprintf(playerMetricSQL, "WHERE p.substation_id = ANY($1::varchar[])", "WHERE s.id = ANY($1::varchar[])")

const playerMetricSQL = `
WITH player_metric AS (
  SELECT p.id, p.guild_id, p.substation_id,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'capacity'), 0) AS capacity,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'load'), 0)
           + COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'structsLoad'), 0) AS load
  FROM structs.player p
  LEFT JOIN structs.grid gr ON gr.object_id = p.id
    AND gr.attribute_type IN ('capacity', 'load', 'structsLoad')
  %s
  GROUP BY p.id, p.guild_id, p.substation_id
),
substation_metric AS (
  SELECT s.id,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'load'), 0) AS load,
         COALESCE(MAX(gr.val) FILTER (WHERE gr.attribute_type = 'connectionCapacity'), 0) AS connection_capacity
  FROM structs.substation s
  LEFT JOIN structs.grid gr ON gr.object_id = s.id
    AND gr.attribute_type IN ('load', 'connectionCapacity')
  %s
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
	// An unnamed extended-protocol execution plans for this ID array on every
	// call. A cached generic plan estimates nearly all players even for a small
	// guild, scanning unrelated grid rows. Keep the mode local to this aggregate.
	_, err := tx.Exec(ctx, guildMetricCTE+`
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
GROUP BY g.id, gm.name, g.name, gb.collateral, gb.supply`, pgx.QueryExecModeExec, ids)
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
	// Group sizes are skewed; avoid retaining a generic plan across ID arrays.
	_, err := tx.Exec(ctx, substationMetricCTE+`
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
GROUP BY s.id, s.owner, sm.load, sm.connection_capacity`, pgx.QueryExecModeExec, ids)
	return err
}
