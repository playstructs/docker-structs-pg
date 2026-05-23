package events

import (
	"context"
	"encoding/json"
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
type structDefenderClearHandler struct{}

func (structDefenderClearHandler) CompositeKey() string {
	return "structs.structs.EventStructDefenderClear.structDefenderClearDetail"
}

const structDefenderClearDefenderSQL = `
DELETE FROM structs.struct_defender WHERE defending_struct_id = $1`

const structDefenderClearAttributeSQL = `
DELETE FROM structs.struct_attribute WHERE id = '5-' || $1`

func (structDefenderClearHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.StructDefenderClear](raw)
	if err != nil {
		return err
	}
	if p.DefendingStructID == "" {
		return fmt.Errorf("struct_defender_clear: empty defending_struct_id")
	}
	if _, err := tx.Exec(ctx, structDefenderClearDefenderSQL, p.DefendingStructID); err != nil {
		return fmt.Errorf("struct_defender_clear defender delete def=%s: %w", p.DefendingStructID, err)
	}
	if _, err := tx.Exec(ctx, structDefenderClearAttributeSQL, p.DefendingStructID); err != nil {
		return fmt.Errorf("struct_defender_clear attribute delete def=%s: %w", p.DefendingStructID, err)
	}
	return nil
}
