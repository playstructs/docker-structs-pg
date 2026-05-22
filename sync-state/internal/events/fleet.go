package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// fleetHandler ports cache.handle_event_fleet
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:321-430).
// UPSERT structs.fleet + structs.player_object(id, owner).
//
// The fleet's map column is a single jsonb assembled from the four ambit
// sub-objects (space/air/land/water). We build it in Go so we can compare
// it with structs.fleet.map via IS DISTINCT FROM and skip no-op updates.
//
// Also emits the fleet_depart + fleet_arrive planet_activity rows that
// the dropped PLANET_ACTIVITY_FLEET_MOVE trigger (cache-system.sql:1201-1262)
// used to write. The Go port fixes TWO long-standing bugs in the SQL
// trigger — see emitFleetMoveActivity.
type fleetHandler struct{}

func (fleetHandler) CompositeKey() string {
	return "structs.structs.EventFleet.fleet"
}

const fleetUpsertSQL = `
INSERT INTO structs.fleet (
    id, owner, map,
    space_slots, air_slots, land_slots, water_slots,
    location_type, location_id, status,
    location_list_forward, location_list_backward, command_struct,
    created_at, updated_at
) VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET owner                 = EXCLUDED.owner,
       map                   = EXCLUDED.map,
       location_type         = EXCLUDED.location_type,
       location_id           = EXCLUDED.location_id,
       status                = EXCLUDED.status,
       location_list_forward = EXCLUDED.location_list_forward,
       location_list_backward = EXCLUDED.location_list_backward,
       command_struct        = EXCLUDED.command_struct,
       updated_at            = NOW()
 WHERE structs.fleet.owner                  IS DISTINCT FROM EXCLUDED.owner
    OR structs.fleet.map                    IS DISTINCT FROM EXCLUDED.map
    OR structs.fleet.location_type          IS DISTINCT FROM EXCLUDED.location_type
    OR structs.fleet.location_id            IS DISTINCT FROM EXCLUDED.location_id
    OR structs.fleet.status                 IS DISTINCT FROM EXCLUDED.status
    OR structs.fleet.location_list_forward  IS DISTINCT FROM EXCLUDED.location_list_forward
    OR structs.fleet.location_list_backward IS DISTINCT FROM EXCLUDED.location_list_backward
    OR structs.fleet.command_struct         IS DISTINCT FROM EXCLUDED.command_struct`

const fleetPrevSelectSQL = `
SELECT location_id, status
  FROM structs.fleet
 WHERE id = $1`

func (fleetHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Fleet](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("fleet: empty id")
	}

	var (
		prevExists              bool
		prevLocID, prevStatusDB *string
	)
	err = tx.QueryRow(ctx, fleetPrevSelectSQL, p.ID).Scan(&prevLocID, &prevStatusDB)
	switch {
	case err == nil:
		prevExists = true
	case errors.Is(err, pgx.ErrNoRows):
		// fresh fleet; no movement to emit
	default:
		return fmt.Errorf("fleet prev id=%s: %w", p.ID, err)
	}

	mapJSON := buildAmbitMap(p.Space, p.Air, p.Land, p.Water)
	if _, err := tx.Exec(ctx, fleetUpsertSQL,
		p.ID,
		payload.NullableText(p.Owner),
		string(mapJSON),
		p.SpaceSlots.Int64(),
		p.AirSlots.Int64(),
		p.LandSlots.Int64(),
		p.WaterSlots.Int64(),
		payload.NullableText(p.LocationType),
		payload.NullableText(p.LocationID),
		payload.NullableText(p.Status),
		payload.NullableText(p.LocationListForward),
		payload.NullableText(p.LocationListBackward),
		payload.NullableText(p.CommandStruct),
	); err != nil {
		return fmt.Errorf("fleet upsert id=%s: %w", p.ID, err)
	}
	if err := upsertPlayerObject(ctx, tx, p.ID, p.Owner); err != nil {
		return err
	}

	if prevExists {
		prevLoc := derefStr(prevLocID)
		prevStat := derefStr(prevStatusDB)
		if err := emitFleetMoveActivity(ctx, tx, bctx, p, prevLoc, prevStat); err != nil {
			return fmt.Errorf("fleet: emit move id=%s: %w", p.ID, err)
		}
	}
	return nil
}

// emitFleetMoveActivity ports cache.PLANET_ACTIVITY_FLEET_MOVE
// (cache-system.sql:1201-1262), with TWO intentional fixes to bugs
// in the original SQL — both clearly typos that the existing UI silently
// tolerates:
//
//  1. The depart-side seq was sourced from NEW.location_id (the arrival
//     planet) but assigned to planet_id = OLD.location_id (the departure
//     planet). Result: the departure planet's seq counter never
//     advanced, and multiple departs from the same planet collided on
//     identical seqs.  We fix it: seq is per-planet (matches planet_id).
//
//  2. The arrive-side fleet_list enrichment (recursive CTE) was assigned
//     `INTO old_move_detail` instead of `new_move_detail`. The INSERT
//     for fleet_arrive then used the un-enriched `new_move_detail`,
//     so fleet_arrive events for `status='away'` arrivals were always
//     missing fleet_list. We fix it: the enrichment lands on the
//     correct variable.
//
// Both fixes are additive (more accurate seq, more complete detail);
// nothing downstream should regress from them.
func emitFleetMoveActivity(ctx context.Context, tx pgx.Tx, bctx BlockContext, p payload.Fleet, prevLocID, prevStatus string) error {
	// SQL guard: OLD.location_id IS NOT NULL AND OLD.location_id <> ''
	// AND (OLD.location_id <> NEW.location_id)
	if prevLocID == "" || prevLocID == p.LocationID {
		return nil
	}
	if err := emitFleetMoveSide(ctx, tx, bctx, p.ID, prevLocID, prevStatus, "fleet_depart"); err != nil {
		return fmt.Errorf("depart: %w", err)
	}
	if err := emitFleetMoveSide(ctx, tx, bctx, p.ID, p.LocationID, p.Status, "fleet_arrive"); err != nil {
		return fmt.Errorf("arrive: %w", err)
	}
	return nil
}

const fleetMoveInsertSQL = `
INSERT INTO structs.planet_activity (time, seq, planet_id, category, detail, block_height)
VALUES ($1, $2, $3, $4, $5::jsonb, $6)`

// fleetMoveListSQL walks the per-planet linked list of "away" fleets
// starting from the queue head (location_list_forward = ''). Mirrors
// the WITH RECURSIVE in PLANET_ACTIVITY_FLEET_MOVE. We do this only
// when fleet_status='away' and we're emitting either side of a move.
const fleetMoveListSQL = `
WITH RECURSIVE r_fleets AS (
    SELECT id, location_list_backward
      FROM structs.fleet
     WHERE location_id = $1 AND location_list_forward = '' AND status = 'away'
    UNION
    SELECT e.id, e.location_list_backward
      FROM structs.fleet e
      JOIN r_fleets s ON s.location_list_backward = e.id
)
SELECT COALESCE(array_agg(id), ARRAY[]::varchar[]) FROM r_fleets`

func emitFleetMoveSide(ctx context.Context, tx pgx.Tx, bctx BlockContext, fleetID, planetID, status, category string) error {
	if planetID == "" {
		// nothing to anchor the activity on (NEW.location_id may be
		// empty if the fleet was just removed). SQL would NOT NULL-trip;
		// we skip cleanly.
		return nil
	}
	detail := map[string]any{
		"fleet_id":     fleetID,
		"fleet_status": status,
	}
	if status == "away" {
		var ids []string
		if err := tx.QueryRow(ctx, fleetMoveListSQL, planetID).Scan(&ids); err != nil {
			return fmt.Errorf("fleet_list lookup planet=%s: %w", planetID, err)
		}
		if len(ids) > 0 {
			detail["fleet_list"] = ids
		}
	}
	seq, err := nextPlanetActivitySeq(ctx, tx, planetID)
	if err != nil {
		return fmt.Errorf("seq: %w", err)
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("detail marshal: %w", err)
	}
	if _, err := tx.Exec(ctx, fleetMoveInsertSQL,
		bctx.BlockTime.UTC(),
		seq,
		planetID,
		category,
		detailJSON,
		bctx.Height,
	); err != nil {
		return err
	}
	return nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// buildAmbitMap assembles {space, air, land, water} as a JSON object.
// Mirrors the SQL handler's jsonb_build_object('space', v.space) || ...
// pattern. nil sub-values become JSON null literals.
func buildAmbitMap(space, air, land, water json.RawMessage) []byte {
	m := map[string]json.RawMessage{
		"space": ensureRaw(space),
		"air":   ensureRaw(air),
		"land":  ensureRaw(land),
		"water": ensureRaw(water),
	}
	return payload.MustMarshal(m)
}

func ensureRaw(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("null")
	}
	return r
}
