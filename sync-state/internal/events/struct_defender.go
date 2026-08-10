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
// Single UPSERT on defending_struct_id. is_planetary is derived from the
// protected struct's location_type (true iff it exists and sits on a
// planet), matching table-struct-defender-20260810-add-is-planetary.
// location_type is immutable per struct, so the flag set here stays
// correct for the row's life. IS DISTINCT FROM guards on both
// protected_struct_id and is_planetary to suppress no-op updates.
type structDefenderHandler struct{}

func (structDefenderHandler) CompositeKey() string {
	return "structs.structs.EventStructDefender.structDefender"
}

const structDefenderUpsertSQL = `
INSERT INTO structs.struct_defender (
    defending_struct_id, protected_struct_id, is_planetary, updated_at
) VALUES ($1, $2::text, COALESCE(
        (SELECT s.location_type = 'planet' FROM structs.struct s WHERE s.id = $2::text), FALSE), NOW())
ON CONFLICT (defending_struct_id) DO UPDATE
   SET protected_struct_id = EXCLUDED.protected_struct_id,
       is_planetary        = EXCLUDED.is_planetary,
       updated_at          = NOW()
 WHERE structs.struct_defender.protected_struct_id IS DISTINCT FROM EXCLUDED.protected_struct_id
    OR structs.struct_defender.is_planetary        IS DISTINCT FROM EXCLUDED.is_planetary`

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
