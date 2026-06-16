package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/payload"
)

// raidHandler ports cache.handle_event_raid — final form in
// cache-trigger-add-queue-20260226-add-seized-ore-better.sql:5-43.
//
// Upserts a single structs.planet_raid row keyed on planet_id. The
// `IS DISTINCT FROM` guard fires only on (fleet_id, status) changes,
// matching the SQL exactly — seized_ore-only updates are intentionally
// not gated.
//
// PLANET_ACTIVITY: emits the timeline row directly. The dropped
// PLANET_ACTIVITY_RAID_STATUS trigger (cache-system.sql:1196) used to do
// this. We emit ONLY when the upsert actually wrote/changed a row
// (matching PG's AFTER INSERT OR UPDATE semantics — UPDATEs whose WHERE
// guard filtered them out do NOT fire the trigger in PG, so we use
// rows-affected to mirror that behavior exactly).
type raidHandler struct{}

func (raidHandler) CompositeKey() string {
	return "structs.structs.EventRaid.eventRaidDetail"
}

const raidUpsertSQL = `
INSERT INTO structs.planet_raid (fleet_id, planet_id, status, seized_ore, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (planet_id) DO UPDATE
   SET fleet_id   = EXCLUDED.fleet_id,
       status     = EXCLUDED.status,
       seized_ore = EXCLUDED.seized_ore,
       updated_at = EXCLUDED.updated_at
 WHERE structs.planet_raid.fleet_id IS DISTINCT FROM EXCLUDED.fleet_id
    OR structs.planet_raid.status   IS DISTINCT FROM EXCLUDED.status`

func (raidHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Raid](raw)
	if err != nil {
		return err
	}
	if p.PlanetID == "" {
		return fmt.Errorf("raid: empty planetId")
	}
	tag, err := tx.Exec(ctx, raidUpsertSQL,
		nullIfEmpty(p.FleetID),
		p.PlanetID,
		nullIfEmpty(p.Status),
		p.SeizedOre.PgValue(),
	)
	if err != nil {
		return fmt.Errorf("raid upsert planet=%s: %w", p.PlanetID, err)
	}

	if tag.RowsAffected() > 0 {
		if err := emitRaidStatusActivity(ctx, tx, bctx, p); err != nil {
			return fmt.Errorf("raid: emit planet_activity planet=%s: %w", p.PlanetID, err)
		}
	}
	return nil
}

// emitRaidStatusActivity ports cache.PLANET_ACTIVITY_RAID_STATUS
// (cache-system.sql:1182-1194). One planet_activity row whose detail
// jsonb mirrors the trigger's jsonb_build_object exactly. Uses
// bctx.BlockTime instead of NOW() so replays and backfills land at the
// correct historical timestamp (TimescaleDB hypertable partitioning).
//
// The detail also folds in the planet's current block_start_raid value (the
// sibling blockStartRaid attribute, id "10-" || planet_id — the same
// "10-" || planet.id surfaced by the planet view). It reads as 0 when no
// raid block is set, mirroring the view's COALESCE(..., 0).
func emitRaidStatusActivity(ctx context.Context, tx pgx.Tx, bctx BlockContext, p payload.Raid) error {
	seq, err := nextPlanetActivitySeq(ctx, tx, p.PlanetID)
	if err != nil {
		return fmt.Errorf("seq: %w", err)
	}
	blockStartRaidID := strconv.Itoa(planetAttrTypeBlockStartRaid) + "-" + p.PlanetID
	blockStartRaid, err := planetAttributePrevVal(ctx, tx, blockStartRaidID)
	if err != nil {
		return fmt.Errorf("block_start_raid planet=%s: %w", p.PlanetID, err)
	}
	detail, err := json.Marshal(map[string]any{
		"planet_id":        p.PlanetID,
		"fleet_id":         p.FleetID,
		"status":           p.Status,
		"block_start_raid": blockStartRaid,
	})
	if err != nil {
		return fmt.Errorf("detail marshal: %w", err)
	}
	bctx.Buf.PlanetActivity = append(bctx.Buf.PlanetActivity, buffers.PlanetActivityRow{
		Time:        bctx.BlockTime.UTC(),
		Seq:         int64(seq),
		PlanetID:    p.PlanetID,
		Category:    "raid_status",
		Detail:      detail,
		BlockHeight: bctx.Height,
	})
	return nil
}
