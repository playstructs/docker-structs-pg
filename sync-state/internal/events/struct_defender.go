package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// structDefenderHandler ports cache.handle_event_struct_defender
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:829-863).
//
// Single UPSERT on defending_struct_id. IS DISTINCT FROM guards on
// protected_struct_id to suppress no-op updates.
type structDefenderHandler struct{}

func (structDefenderHandler) CompositeKey() string {
	return "structs.structs.EventStructDefender.structDefender"
}

const structDefenderUpsertSQL = `
INSERT INTO structs.struct_defender (
    defending_struct_id, protected_struct_id, updated_at
) VALUES ($1, $2, NOW())
ON CONFLICT (defending_struct_id) DO UPDATE
   SET protected_struct_id = EXCLUDED.protected_struct_id,
       updated_at          = NOW()
 WHERE structs.struct_defender.protected_struct_id IS DISTINCT FROM EXCLUDED.protected_struct_id`

func (structDefenderHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.StructDefender](raw)
	if err != nil {
		return err
	}
	if p.DefendingStructID == "" {
		return fmt.Errorf("struct_defender: empty defending_struct_id")
	}
	if _, err := tx.Exec(ctx, structDefenderUpsertSQL,
		p.DefendingStructID,
		payload.NullableText(p.ProtectedStructID),
	); err != nil {
		return fmt.Errorf("struct_defender upsert def=%s: %w", p.DefendingStructID, err)
	}
	return nil
}
