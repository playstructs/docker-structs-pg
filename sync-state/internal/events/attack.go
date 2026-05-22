package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// attackHandler ports cache.handle_event_attack
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1961-1988).
//
// One planet_activity row, category='struct_attack', whose detail jsonb
// is the FULL raw payload (we lose nothing the chain emitted). The planet
// is resolved from the attacker's struct via getActivityLocationID
// (Go port of structs.GET_ACTIVITY_LOCATION_ID).
//
// If the planet can't be resolved (attacker struct hasn't been seen by
// the indexer yet — typical during a backfill that's still catching up
// to entity events), we skip the INSERT cleanly. The SQL handler would
// try to INSERT a row with NULL planet_id and fail the NOT NULL
// constraint on structs.planet_activity.planet_id (table-planet.sql:80);
// the failure was swallowed by cache.ADD_QUEUE's EXCEPTION block, so the
// row was effectively dropped anyway. We do the same skip explicitly,
// without poking the error path.
//
// Stat / health / destruction follow-ups for the defender come from
// Phase 4's struct_attribute handler (status bit 32). Phase 5 attack is
// just the "this happened" timeline row.
type attackHandler struct{}

func (attackHandler) CompositeKey() string {
	return "structs.structs.EventAttack.eventAttackDetail"
}

const attackInsertSQL = `
INSERT INTO structs.planet_activity (time, seq, planet_id, category, detail, block_height)
VALUES ($1, $2, $3, 'struct_attack', $4::jsonb, $5)`

func (attackHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Attack](raw)
	if err != nil {
		return err
	}
	if p.AttackerStructID == "" {
		return fmt.Errorf("attack: empty attackerStructId")
	}

	planetID, err := getActivityLocationID(ctx, tx, p.AttackerStructID)
	if err != nil {
		return fmt.Errorf("attack: resolve planet: %w", err)
	}
	if planetID == "" {
		// Unknown attacker struct → planet_id would be NULL.
		// planet_activity.planet_id is NOT NULL; SQL handler also fails
		// here (silently, via EXCEPTION). Skip cleanly.
		return nil
	}

	seq, err := nextPlanetActivitySeq(ctx, tx, planetID)
	if err != nil {
		return fmt.Errorf("attack: seq: %w", err)
	}

	if _, err := tx.Exec(ctx, attackInsertSQL,
		bctx.BlockTime.UTC(),
		seq,
		planetID,
		[]byte(raw),
		bctx.Height,
	); err != nil {
		return fmt.Errorf("attack insert struct=%s planet=%s: %w", p.AttackerStructID, planetID, err)
	}
	return nil
}
