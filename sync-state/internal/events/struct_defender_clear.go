package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// structDefenderClearHandler ports cache.handle_event_struct_defender_clear
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:865-883).
//
// Always deletes TWO rows:
//   1. structs.struct_defender WHERE defending_struct_id = $1
//   2. structs.struct_attribute WHERE id = '5-' || $1
//
// The struct_attribute side-effect mirrors the SQL handler exactly. The
// '5-' prefix is the attribute type id for defender-related grid state
// (see structs.GET_OBJECT_TYPE). Phase 4's struct_attribute handler also
// touches these ids — we don't coordinate with it here because both
// handlers are idempotent DELETEs/UPSERTs of distinct row sets.
//
// PLANET_ACTIVITY: this is the ONLY path that clears a defender's
// protectedStructIndex attribute (the chain does not also emit an
// EventStructAttribute delete — confirmed by the live DB carrying 0
// struct_defense_remove rows despite many clears). The Phase 4
// struct_attribute handler's attrType==5 delete branch therefore never
// runs for a clear, so we emit the struct_defense_remove timeline row
// here, reusing emitStructDefenseRemove for an identical detail shape.
// We read the protectedStructIndex val before deleting so we know which
// protected struct's planet to anchor the activity on.
type structDefenderClearHandler struct{}

func (structDefenderClearHandler) CompositeKey() string {
	return "structs.structs.EventStructDefenderClear.structDefenderClearDetail"
}

const structDefenderClearDefenderSQL = `
DELETE FROM structs.struct_defender WHERE defending_struct_id = $1`

const structDefenderClearAttributeSQL = `
DELETE FROM structs.struct_attribute WHERE id = '5-' || $1`

const structDefenderClearPrevValSQL = `
SELECT val FROM structs.struct_attribute WHERE id = '5-' || $1`

func (structDefenderClearHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.StructDefenderClear](raw)
	if err != nil {
		return err
	}
	if p.DefendingStructID == "" {
		return fmt.Errorf("struct_defender_clear: empty defending_struct_id")
	}

	// Capture the protectedStructIndex val before the delete so the
	// struct_defense_remove emit knows which protected struct to anchor on.
	var protectedIndex int64
	if err := tx.QueryRow(ctx, structDefenderClearPrevValSQL, p.DefendingStructID).Scan(&protectedIndex); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("struct_defender_clear prev val def=%s: %w", p.DefendingStructID, err)
	}

	if _, err := tx.Exec(ctx, structDefenderClearDefenderSQL, p.DefendingStructID); err != nil {
		return fmt.Errorf("struct_defender_clear defender delete def=%s: %w", p.DefendingStructID, err)
	}
	tag, err := tx.Exec(ctx, structDefenderClearAttributeSQL, p.DefendingStructID)
	if err != nil {
		return fmt.Errorf("struct_defender_clear attribute delete def=%s: %w", p.DefendingStructID, err)
	}

	if tag.RowsAffected() > 0 && protectedIndex > 0 {
		if err := emitStructDefenseRemove(ctx, tx, bctx, protectedIndex, p.DefendingStructID); err != nil {
			return fmt.Errorf("struct_defender_clear defense_remove def=%s: %w", p.DefendingStructID, err)
		}
	}
	return nil
}
