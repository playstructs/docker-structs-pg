package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// providerHandler ports cache.handle_event_provider
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:606-700).
//
// rate is the chain's Coin-shaped {amount, denom} object; the SQL handler
// destructures it into rate_amount / rate_denom and we do the same.
type providerHandler struct{}

func (providerHandler) CompositeKey() string {
	return "structs.structs.EventProvider.provider"
}

const providerUpsertSQL = `
INSERT INTO structs.provider (
    id, index, substation_id,
    rate_amount, rate_denom,
    access_policy,
    capacity_minimum, capacity_maximum,
    duration_minimum, duration_maximum,
    provider_cancellation_penalty, consumer_cancellation_penalty,
    creator, owner,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET access_policy    = EXCLUDED.access_policy,
       capacity_minimum = EXCLUDED.capacity_minimum,
       capacity_maximum = EXCLUDED.capacity_maximum,
       duration_minimum = EXCLUDED.duration_minimum,
       duration_maximum = EXCLUDED.duration_maximum,
       updated_at       = NOW()
 WHERE structs.provider.access_policy    IS DISTINCT FROM EXCLUDED.access_policy
    OR structs.provider.capacity_minimum IS DISTINCT FROM EXCLUDED.capacity_minimum
    OR structs.provider.capacity_maximum IS DISTINCT FROM EXCLUDED.capacity_maximum
    OR structs.provider.duration_minimum IS DISTINCT FROM EXCLUDED.duration_minimum
    OR structs.provider.duration_maximum IS DISTINCT FROM EXCLUDED.duration_maximum`

func (providerHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Provider](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("provider: empty id")
	}
	if _, err := tx.Exec(ctx, providerUpsertSQL,
		p.ID,
		p.Index.Int64(),
		payload.NullableText(p.SubstationID),
		p.Rate.Amount.PgValue(),
		payload.NullableText(p.Rate.Denom),
		payload.NullableText(p.AccessPolicy),
		p.CapacityMinimum.PgValue(),
		p.CapacityMaximum.PgValue(),
		p.DurationMinimum.PgValue(),
		p.DurationMaximum.PgValue(),
		p.ProviderCancellationPenalty.PgValue(),
		p.ConsumerCancellationPenalty.PgValue(),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Owner),
	); err != nil {
		return fmt.Errorf("provider upsert id=%s: %w", p.ID, err)
	}
	return upsertPlayerObject(ctx, tx, p.ID, p.Owner)
}
