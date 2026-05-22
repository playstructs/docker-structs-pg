package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// allocationHandler ports cache.handle_event_allocation
// (cache-trigger-add-queue-20260325-110b-schema-update.sql:5-62).
// Single-table UPSERT, no sidecar.
type allocationHandler struct{}

func (allocationHandler) CompositeKey() string {
	return "structs.structs.EventAllocation.allocation"
}

const allocationUpsertSQL = `
INSERT INTO structs.allocation (
    id, allocation_type, source_id, index, destination_id, creator, controller,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET destination_id = EXCLUDED.destination_id,
       controller     = EXCLUDED.controller,
       updated_at     = NOW()
 WHERE structs.allocation.destination_id IS DISTINCT FROM EXCLUDED.destination_id
    OR structs.allocation.controller     IS DISTINCT FROM EXCLUDED.controller`

func (allocationHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Allocation](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("allocation: empty id")
	}
	if _, err := tx.Exec(ctx, allocationUpsertSQL,
		p.ID,
		payload.NullableText(p.Type),
		payload.NullableText(p.SourceObjectID),
		p.Index.Int64(),
		payload.NullableText(p.DestinationID),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Controller),
	); err != nil {
		return fmt.Errorf("allocation upsert id=%s: %w", p.ID, err)
	}
	return nil
}
