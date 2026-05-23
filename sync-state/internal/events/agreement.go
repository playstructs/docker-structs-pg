package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// agreementHandler ports cache.handle_event_agreement
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:92-160).
// UPSERT structs.agreement + structs.player_object(id, owner) sidecar.
type agreementHandler struct{}

func (agreementHandler) CompositeKey() string {
	return "structs.structs.EventAgreement.agreement"
}

const agreementUpsertSQL = `
INSERT INTO structs.agreement (
    id, provider_id, allocation_id, capacity, start_block, end_block,
    creator, owner, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET capacity    = EXCLUDED.capacity,
       start_block = EXCLUDED.start_block,
       end_block   = EXCLUDED.end_block,
       updated_at  = NOW()
 WHERE structs.agreement.capacity    IS DISTINCT FROM EXCLUDED.capacity
    OR structs.agreement.start_block IS DISTINCT FROM EXCLUDED.start_block
    OR structs.agreement.end_block   IS DISTINCT FROM EXCLUDED.end_block`

func (agreementHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Agreement](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("agreement: empty id")
	}
	if _, err := tx.Exec(ctx, agreementUpsertSQL,
		p.ID,
		payload.NullableText(p.ProviderID),
		payload.NullableText(p.AllocationID),
		p.Capacity.Int64(),
		p.StartBlock.Int64(),
		p.EndBlock.Int64(),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Owner),
	); err != nil {
		return fmt.Errorf("agreement upsert id=%s: %w", p.ID, err)
	}
	return upsertPlayerObject(ctx, tx, p.ID, p.Owner)
}
