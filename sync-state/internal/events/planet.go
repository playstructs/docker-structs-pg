package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// planetHandler ports cache.handle_event_planet
// (cache-trigger-add-queue-20260427-ugc-fields.sql:312-419).
// UPSERT structs.planet + structs.player_object + planet_meta seeding +
// conditional structs.planet_meta.name UPDATE (only when chain sends
// non-empty name).
//
// planet_meta seeding (INSERT) is owned by sync-state — the legacy
// NAME_PLANET trigger has been dropped (see Phase B SQL).
type planetHandler struct{}

func (planetHandler) CompositeKey() string {
	return "structs.structs.EventPlanet.planet"
}

const planetUpsertSQL = `
INSERT INTO structs.planet (
    id, max_ore, creator, owner, map,
    space_slots, air_slots, land_slots, water_slots,
    status, location_list_start, location_list_end,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET owner               = EXCLUDED.owner,
       map                 = EXCLUDED.map,
       status              = EXCLUDED.status,
       location_list_start = EXCLUDED.location_list_start,
       location_list_end   = EXCLUDED.location_list_end,
       updated_at          = NOW()
 WHERE structs.planet.owner               IS DISTINCT FROM EXCLUDED.owner
    OR structs.planet.map                 IS DISTINCT FROM EXCLUDED.map
    OR structs.planet.status              IS DISTINCT FROM EXCLUDED.status
    OR structs.planet.location_list_start IS DISTINCT FROM EXCLUDED.location_list_start
    OR structs.planet.location_list_end   IS DISTINCT FROM EXCLUDED.location_list_end`

// planetMetaNameUpdateSQL matches the SQL handler exactly: only update
// when chain sent a non-empty name AND it differs from existing.
const planetMetaNameUpdateSQL = `
UPDATE structs.planet_meta
   SET name       = $2,
       updated_at = NOW()
 WHERE id = $1
   AND name IS DISTINCT FROM $2`

// planetMetaSeedSQL improves on the dropped NAME_PLANET trigger: skips
// the INSERT entirely when player.guild_id is NULL or the player row
// doesn't exist yet.
//
// The SQL trigger DID NOT do this — it tried to insert (planet.id,
// NULL), and the planet_meta PRIMARY KEY (id, guild_id) treats NULL as
// non-distinct for PK purposes and rejects with SQLSTATE 23502 (NOT
// NULL violation on guild_id). That failure cascaded up the AFTER
// INSERT trigger and rolled back the planet INSERT entirely — observed
// in production at h=793782 ("planet upsert id=2-193: null value in
// column guild_id").
//
// We do the equivalent of WHERE guild_id IS NOT NULL: the SELECT
// returns 0 rows when the player doesn't exist OR has no guild, and
// INSERT...SELECT inserts 0 rows. No constraint trip. A later
// playerHandler upsert can then seed via planetMetaBackfillForPlayerSQL.
//
// The NOT EXISTS guard is load-bearing for the steady-state hot path:
// planet_meta.name DEFAULT is generate_planet_name() which loops over
// structs.banned_word for every candidate row. PG evaluates the DEFAULT
// for EVERY candidate row the SELECT emits — including ones the ON
// CONFLICT DO NOTHING will reject — so a "no-op" re-INSERT for an
// already-present (planet, guild) was paying the function cost on
// every block. With the anti-join, we don't emit the candidate at all
// when the row exists, so the DEFAULT is never evaluated. Confirmed
// against EXPLAIN ANALYZE: 30ms -> 0.6ms, 632 buffer hits -> 4. This
// also closes the hang we saw at h=773548 where one such INSERT got
// wedged for 29 minutes under TimescaleDB chunk lock contention.
// $1 is referenced in both the SELECT projection (target column
// planet_meta.id, varchar) AND the NOT EXISTS subquery's pm.id = $1
// (also varchar). pgx's prepared-statement type inference fails with
// SQLSTATE 42P08 ("inconsistent types deduced for parameter $1") if we
// leave both bare, so we cast the WHERE-side to text. The SELECT-side
// will be assignment-coerced to varchar by the INSERT.
const planetMetaSeedSQL = `
INSERT INTO structs.planet_meta (id, guild_id, created_at, updated_at)
SELECT ($1)::text, p.guild_id, NOW(), NOW()
  FROM structs.player p
 WHERE p.id = $2 AND p.guild_id IS NOT NULL
   AND NOT EXISTS (
       SELECT 1 FROM structs.planet_meta pm
        WHERE pm.id = ($1)::text AND pm.guild_id = p.guild_id
   )
ON CONFLICT (id, guild_id) DO NOTHING`

func (planetHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Planet](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("planet: empty id")
	}
	mapJSON := buildAmbitMap(p.Space, p.Air, p.Land, p.Water)
	if _, err := tx.Exec(ctx, planetUpsertSQL,
		p.ID,
		p.MaxOre.Int64(),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Owner),
		string(mapJSON),
		p.SpaceSlots.Int64(),
		p.AirSlots.Int64(),
		p.LandSlots.Int64(),
		p.WaterSlots.Int64(),
		payload.NullableText(p.Status),
		payload.NullableText(p.LocationListStart),
		payload.NullableText(p.LocationListEnd),
	); err != nil {
		return fmt.Errorf("planet upsert id=%s: %w", p.ID, err)
	}
	if err := upsertPlayerObject(ctx, tx, p.ID, p.Owner); err != nil {
		return err
	}

	if p.Owner != "" {
		if _, err := tx.Exec(ctx, planetMetaSeedSQL, p.ID, p.Owner); err != nil {
			return fmt.Errorf("planet_meta seed id=%s: %w", p.ID, err)
		}
	}

	if p.Name != "" {
		if _, err := tx.Exec(ctx, planetMetaNameUpdateSQL, p.ID, p.Name); err != nil {
			return fmt.Errorf("planet_meta name id=%s: %w", p.ID, err)
		}
	}
	return nil
}
