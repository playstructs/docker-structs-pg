package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
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

func (oreMigrateHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.OreMigrate](raw)
	if err != nil {
		return err
	}
	if p.PrimaryAddress == "" || p.OldPrimaryAddress == "" {
		return fmt.Errorf("ore_migrate: missing address (new=%q old=%q)", p.PrimaryAddress, p.OldPrimaryAddress)
	}
	amt := p.Amount.String()
	t := bctx.BlockTime.UTC()
	bctx.Buf.Ledger = append(bctx.Buf.Ledger,
		buffers.LedgerRow{Address: p.PrimaryAddress, Counterparty: p.OldPrimaryAddress, AmountP: amt, BlockHeight: bctx.Height, Time: t, Action: "migrated", Direction: "credit", Denom: "ore"},
		buffers.LedgerRow{Address: p.OldPrimaryAddress, Counterparty: p.PrimaryAddress, AmountP: amt, BlockHeight: bctx.Height, Time: t, Action: "migrated", Direction: "debit", Denom: "ore"},
	)
	_ = tx
	return nil
}
