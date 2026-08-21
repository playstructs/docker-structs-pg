package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// guildBankAddressHandler ports cache.handle_event_guild_bank_address
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2210-2249).
//
// Writes TWO address_tag rows for the bankCollateralPool address:
//
//	Type='Bank Collateral Pool'
//	GuildId=$guildId
//
// Same upsert + IS DISTINCT FROM guard as provider_address.
type guildBankAddressHandler struct{}

func (guildBankAddressHandler) CompositeKey() string {
	return "structs.structs.EventGuildBankAddress.eventGuildBankAddressDetail"
}

const guildBankAddressUpsertSQL = `
INSERT INTO structs.address_tag (address, label, entry, updated_at, created_at) VALUES
    ($1, 'Type',    'Bank Collateral Pool', NOW(), NOW()),
    ($1, 'GuildId', $2,                     NOW(), NOW())
ON CONFLICT (address, label) DO UPDATE
   SET entry      = EXCLUDED.entry,
       updated_at = EXCLUDED.updated_at
 WHERE structs.address_tag.entry IS DISTINCT FROM EXCLUDED.entry`

const guildBankPreviousGuildSQL = `
SELECT entry FROM structs.address_tag WHERE address = $1 AND label = 'GuildId'`

func (guildBankAddressHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.GuildBankAddress](raw)
	if err != nil {
		return err
	}
	if p.BankCollateralPool == "" || p.GuildID == "" {
		return fmt.Errorf("guild_bank_address: missing field (pool=%q guildId=%q)",
			p.BankCollateralPool, p.GuildID)
	}
	var previous *string
	err = tx.QueryRow(ctx, guildBankPreviousGuildSQL, p.BankCollateralPool).Scan(&previous)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("guild_bank_address previous guild pool=%s: %w", p.BankCollateralPool, err)
	}
	if _, err := tx.Exec(ctx, guildBankAddressUpsertSQL,
		p.BankCollateralPool,
		p.GuildID,
	); err != nil {
		return fmt.Errorf("guild_bank_address upsert guild=%s: %w", p.GuildID, err)
	}
	if previous != nil {
		bctx.Dirty.Guild(*previous)
	}
	bctx.Dirty.Guild(p.GuildID)
	bctx.Dirty.Address(p.BankCollateralPool)
	return nil
}
