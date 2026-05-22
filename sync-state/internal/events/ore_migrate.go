package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// oreMigrateHandler ports cache.handle_event_ore_migrate
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2012-2037).
//
// Paired transfer in the ledger: same block, same amount, opposite
// direction, cross-referenced counterparties. Both rows must land or
// neither does — we rely on the per-block tx for atomicity.
//
// See ore_mine.go for the bctx.Height / bctx.BlockTime rationale.
type oreMigrateHandler struct{}

func (oreMigrateHandler) CompositeKey() string {
	return "structs.structs.EventOreMigrate.eventOreMigrateDetail"
}

const oreMigrateInsertSQL = `
INSERT INTO structs.ledger (
    address, counterparty, amount_p, block_height, time, action, direction, denom
) VALUES
    ($1, $2, $3, $4, $5, 'migrated', 'credit', 'ore'),
    ($2, $1, $3, $4, $5, 'migrated', 'debit',  'ore')`

func (oreMigrateHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.OreMigrate](raw)
	if err != nil {
		return err
	}
	if p.PrimaryAddress == "" || p.OldPrimaryAddress == "" {
		return fmt.Errorf("ore_migrate: missing address (new=%q old=%q)", p.PrimaryAddress, p.OldPrimaryAddress)
	}
	if _, err := tx.Exec(ctx, oreMigrateInsertSQL,
		p.PrimaryAddress,
		p.OldPrimaryAddress,
		p.Amount.PgValue(),
		bctx.Height,
		bctx.BlockTime.UTC(),
	); err != nil {
		return fmt.Errorf("ore_migrate insert new=%s old=%s: %w", p.PrimaryAddress, p.OldPrimaryAddress, err)
	}
	return nil
}
