package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// providerAddressHandler ports cache.handle_event_provider_address
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2153-2208).
//
// Writes FOUR address_tag rows (2 addresses × 2 labels):
//
//	collateral_pool: Type='Provider Collateral Pool', ProviderId=$providerId
//	earning_pool:    Type='Provider Earning Pool',    ProviderId=$providerId
//
// Single multi-row INSERT with ON CONFLICT DO UPDATE WHERE the entry
// changed — matches the SQL's IS DISTINCT FROM guard exactly.
type providerAddressHandler struct{}

func (providerAddressHandler) CompositeKey() string {
	return "structs.structs.EventProviderAddress.eventProviderAddressDetail"
}

const providerAddressUpsertSQL = `
INSERT INTO structs.address_tag (address, label, entry, updated_at, created_at) VALUES
    ($1, 'Type',       'Provider Collateral Pool', NOW(), NOW()),
    ($1, 'ProviderId', $3,                         NOW(), NOW()),
    ($2, 'Type',       'Provider Earning Pool',    NOW(), NOW()),
    ($2, 'ProviderId', $3,                         NOW(), NOW())
ON CONFLICT (address, label) DO UPDATE
   SET entry      = EXCLUDED.entry,
       updated_at = EXCLUDED.updated_at
 WHERE structs.address_tag.entry IS DISTINCT FROM EXCLUDED.entry`

func (providerAddressHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.ProviderAddress](raw)
	if err != nil {
		return err
	}
	if p.CollateralPool == "" || p.EarningPool == "" || p.ProviderID == "" {
		return fmt.Errorf("provider_address: missing field (collateral=%q earning=%q providerId=%q)",
			p.CollateralPool, p.EarningPool, p.ProviderID)
	}
	if _, err := tx.Exec(ctx, providerAddressUpsertSQL,
		p.CollateralPool,
		p.EarningPool,
		p.ProviderID,
	); err != nil {
		return fmt.Errorf("provider_address upsert provider=%s: %w", p.ProviderID, err)
	}
	return nil
}
