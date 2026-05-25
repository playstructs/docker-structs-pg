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
// UPSERT structs.planet + structs.player_object + conditional
// structs.planet.name UPDATE (only when chain sends non-empty name).
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

// planetNameUpdateSQL matches the SQL handler exactly: only update when
// chain sent a non-empty name AND it differs from existing.
const planetNameUpdateSQL = `
UPDATE structs.planet
   SET name       = $2,
       updated_at = NOW()
 WHERE id = $1
   AND name IS DISTINCT FROM $2`

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

	if p.Name != "" {
		if _, err := tx.Exec(ctx, planetNameUpdateSQL, p.ID, p.Name); err != nil {
			return fmt.Errorf("planet name id=%s: %w", p.ID, err)
		}
	}
	return nil
}
