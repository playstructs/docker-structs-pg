package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// structHandler ports cache.handle_event_struct
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:749-827).
//
// Note: is_destroyed is set to FALSE on first INSERT and never touched on
// UPDATE — the struct_attribute handler (Phase 3) owns destruction state.
//
// Also emits the struct_move planet_activity row that the dropped
// PLANET_ACTIVITY_STRUCT_MOVEMENT trigger (cache-system.sql:1148-1178)
// used to write. See emitStructMovementActivity.
type structHandler struct{}

func (structHandler) CompositeKey() string {
	return "structs.structs.EventStruct.structure"
}

const structUpsertSQL = `
INSERT INTO structs.struct (
    id, index, type, creator, owner,
    location_type, location_id, operating_ambit, slot,
    created_at, updated_at, is_destroyed
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW(), FALSE)
ON CONFLICT (id) DO UPDATE
   SET owner           = EXCLUDED.owner,
       location_type   = EXCLUDED.location_type,
       location_id     = EXCLUDED.location_id,
       operating_ambit = EXCLUDED.operating_ambit,
       slot            = EXCLUDED.slot,
       updated_at      = NOW()
 WHERE structs.struct.owner           IS DISTINCT FROM EXCLUDED.owner
    OR structs.struct.location_type   IS DISTINCT FROM EXCLUDED.location_type
    OR structs.struct.location_id     IS DISTINCT FROM EXCLUDED.location_id
    OR structs.struct.operating_ambit IS DISTINCT FROM EXCLUDED.operating_ambit
    OR structs.struct.slot            IS DISTINCT FROM EXCLUDED.slot`

// structPrevSelectSQL grabs the pre-upsert row so we can detect movement.
const structPrevSelectSQL = `
SELECT location_type, location_id, operating_ambit, slot
  FROM structs.struct
 WHERE id = $1`

func (structHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Struct](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("struct: empty id")
	}

	var (
		prevExists                                bool
		prevLocType, prevLocID, prevOperatingAmbit *string
		prevSlot                                  int64
	)
	err = tx.QueryRow(ctx, structPrevSelectSQL, p.ID).Scan(
		&prevLocType, &prevLocID, &prevOperatingAmbit, &prevSlot,
	)
	switch {
	case err == nil:
		prevExists = true
	case errors.Is(err, pgx.ErrNoRows):
		// fresh struct; no prior row, no movement emit
	default:
		return fmt.Errorf("struct prev id=%s: %w", p.ID, err)
	}

	if _, err := tx.Exec(ctx, structUpsertSQL,
		p.ID,
		p.Index.Int64(),
		p.Type.Int64(),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Owner),
		payload.NullableText(p.LocationType),
		payload.NullableText(p.LocationID),
		payload.NullableText(p.OperatingAmbit),
		p.Slot.Int64(),
	); err != nil {
		return fmt.Errorf("struct upsert id=%s: %w", p.ID, err)
	}
	if err := upsertPlayerObject(ctx, tx, p.ID, p.Owner); err != nil {
		return err
	}

	if prevExists {
		moved := strDiffer(prevLocID, p.LocationID) ||
			strDiffer(prevOperatingAmbit, p.OperatingAmbit) ||
			prevSlot != p.Slot.Int64()
		if moved {
			if err := emitStructMovementActivity(ctx, tx, bctx, p); err != nil {
				return fmt.Errorf("struct: emit movement id=%s: %w", p.ID, err)
			}
		}
	}
	return nil
}

// emitStructMovementActivity ports cache.PLANET_ACTIVITY_STRUCT_MOVEMENT.
// When the struct's new location_type='fleet', we follow the fleet's
// own location_id to get the planet (a struct on a fleet on a planet is
// still "on" that planet for activity-stream purposes).
const structMovementFleetLookupSQL = `SELECT location_id FROM structs.fleet WHERE id = $1`

const structMovementInsertSQL = `
INSERT INTO structs.planet_activity (time, seq, planet_id, category, detail, block_height)
VALUES ($1, $2, $3, 'struct_move', $4::jsonb, $5)`

func emitStructMovementActivity(ctx context.Context, tx pgx.Tx, bctx BlockContext, p payload.Struct) error {
	planetID := p.LocationID
	if p.LocationType == "fleet" && p.LocationID != "" {
		var fleetLoc *string
		err := tx.QueryRow(ctx, structMovementFleetLookupSQL, p.LocationID).Scan(&fleetLoc)
		switch {
		case err == nil:
			if fleetLoc != nil && *fleetLoc != "" {
				planetID = *fleetLoc
			}
		case errors.Is(err, pgx.ErrNoRows):
			// fleet not yet seen; fall back to raw location_id
		default:
			return fmt.Errorf("fleet lookup id=%s: %w", p.LocationID, err)
		}
	}
	if planetID == "" {
		// Nothing to anchor the activity on. SQL trigger would also pass
		// NULL into planet_activity.planet_id here and fail the NOT NULL
		// constraint (rolled back by ADD_QUEUE's EXCEPTION). Skip cleanly.
		return nil
	}
	seq, err := nextPlanetActivitySeq(ctx, tx, planetID)
	if err != nil {
		return fmt.Errorf("seq: %w", err)
	}
	detail, err := json.Marshal(map[string]any{
		"struct_id":     p.ID,
		"location_type": p.LocationType,
		"location_id":   p.LocationID,
		"ambit":         p.OperatingAmbit,
		"slot":          p.Slot.Int64(),
	})
	if err != nil {
		return fmt.Errorf("detail marshal: %w", err)
	}
	if _, err := tx.Exec(ctx, structMovementInsertSQL,
		bctx.BlockTime.UTC(),
		seq,
		planetID,
		detail,
		bctx.Height,
	); err != nil {
		return err
	}
	return nil
}

// strDiffer compares the SQL-trigger semantic "<>" between a column
// (possibly NULL → *string nil) and an incoming payload string.
// Treats NULL and "" as equal — the SQL handler reads OLD/NEW as actual
// values; an unset incoming string corresponds to NULL in DB.
func strDiffer(prev *string, next string) bool {
	if prev == nil {
		return next != ""
	}
	return *prev != next
}
