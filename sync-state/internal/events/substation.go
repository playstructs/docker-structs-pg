package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// substationHandler ports cache.handle_event_substation
// (cache-trigger-add-queue-20260427-ugc-fields.sql:254-304).
//
// name/pfp live directly on structs.substation (no meta table).
type substationHandler struct{}

func (substationHandler) CompositeKey() string {
	return "structs.structs.EventSubstation.substation"
}

const substationUpsertSQL = `
INSERT INTO structs.substation (
    id, owner, creator, name, pfp,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET owner      = EXCLUDED.owner,
       name       = EXCLUDED.name,
       pfp        = EXCLUDED.pfp,
       updated_at = NOW()
 WHERE structs.substation.owner IS DISTINCT FROM EXCLUDED.owner
    OR structs.substation.name  IS DISTINCT FROM EXCLUDED.name
    OR structs.substation.pfp   IS DISTINCT FROM EXCLUDED.pfp`

func (substationHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Substation](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("substation: empty id")
	}
	if _, err := tx.Exec(ctx, substationUpsertSQL,
		p.ID,
		payload.NullableText(p.Owner),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Name),
		payload.NullableText(p.PFP),
	); err != nil {
		return fmt.Errorf("substation upsert id=%s: %w", p.ID, err)
	}
	return upsertPlayerObject(ctx, tx, p.ID, p.Owner)
}
