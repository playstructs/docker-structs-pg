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
    id, owner, creator, name, pfp, deletion_pending, deletion_migration_substation_id,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET owner      = EXCLUDED.owner,
       name       = EXCLUDED.name,
       pfp        = EXCLUDED.pfp,
       deletion_pending = EXCLUDED.deletion_pending,
       deletion_migration_substation_id = EXCLUDED.deletion_migration_substation_id,
       updated_at = NOW()
 WHERE structs.substation.owner IS DISTINCT FROM EXCLUDED.owner
    OR structs.substation.name  IS DISTINCT FROM EXCLUDED.name
    OR structs.substation.pfp   IS DISTINCT FROM EXCLUDED.pfp
    OR structs.substation.deletion_pending IS DISTINCT FROM EXCLUDED.deletion_pending
    OR structs.substation.deletion_migration_substation_id IS DISTINCT FROM EXCLUDED.deletion_migration_substation_id`

func (substationHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeVersioned[payload.Substation](raw, bctx.V022Active(), "deletion_pending", "deletion_migration_substation_id")
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
		p.DeletionPending,
		p.DeletionMigrationSubstationID,
	); err != nil {
		return fmt.Errorf("substation upsert id=%s: %w", p.ID, err)
	}
	if err := upsertPlayerObject(ctx, tx, p.ID, p.Owner); err != nil {
		return err
	}
	bctx.Dirty.Substation(p.ID)
	return nil
}
