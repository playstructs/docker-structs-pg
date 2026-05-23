package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// reactorHandler ports cache.handle_event_reactor
// (cache-trigger-add-queue-20260325-110b-schema-update.sql:237-293).
type reactorHandler struct{}

func (reactorHandler) CompositeKey() string {
	return "structs.structs.EventReactor.reactor"
}

const reactorUpsertSQL = `
INSERT INTO structs.reactor (
    id, validator, guild_id, default_commission, owner,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET guild_id           = EXCLUDED.guild_id,
       default_commission = EXCLUDED.default_commission,
       owner              = EXCLUDED.owner,
       updated_at         = NOW()
 WHERE structs.reactor.guild_id           IS DISTINCT FROM EXCLUDED.guild_id
    OR structs.reactor.default_commission IS DISTINCT FROM EXCLUDED.default_commission
    OR structs.reactor.owner              IS DISTINCT FROM EXCLUDED.owner`

func (reactorHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Reactor](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("reactor: empty id")
	}
	if _, err := tx.Exec(ctx, reactorUpsertSQL,
		p.ID,
		payload.NullableText(p.Validator),
		payload.NullableText(p.GuildID),
		p.DefaultCommission.PgValue(),
		payload.NullableText(p.Owner),
	); err != nil {
		return fmt.Errorf("reactor upsert id=%s: %w", p.ID, err)
	}
	return upsertPlayerObject(ctx, tx, p.ID, p.Owner)
}
