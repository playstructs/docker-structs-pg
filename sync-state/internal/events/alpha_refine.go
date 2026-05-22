package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// alphaRefineHandler ports cache.handle_event_alpha_refine
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2066-2089).
//
// Two ledger rows for the same primaryAddress:
//
//   - `refined` DEBIT  in `ore`    — amount as-is
//   - `refined` CREDIT in `ualpha` — 1_000_000 × amount (micro-units)
//
// We do the multiplication in PG (`$2::numeric * 1000000`) so arbitrary-
// precision NUMERIC math survives the round-trip — a Go float64 of a
// large amount would lose precision.
//
// See ore_mine.go for the bctx.Height / bctx.BlockTime rationale.
type alphaRefineHandler struct{}

func (alphaRefineHandler) CompositeKey() string {
	return "structs.structs.EventAlphaRefine.eventAlphaRefineDetail"
}

const alphaRefineInsertSQL = `
INSERT INTO structs.ledger (
    address, counterparty, amount_p, block_height, time, action, direction, denom
) VALUES
    ($1, NULL, $2::numeric,                 $3, $4, 'refined', 'debit',  'ore'),
    ($1, NULL, $2::numeric * 1000000::numeric, $3, $4, 'refined', 'credit', 'ualpha')`

func (alphaRefineHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.AlphaRefine](raw)
	if err != nil {
		return err
	}
	if p.PrimaryAddress == "" {
		return fmt.Errorf("alpha_refine: empty primaryAddress")
	}
	if _, err := tx.Exec(ctx, alphaRefineInsertSQL,
		p.PrimaryAddress,
		p.Amount.PgValue(),
		bctx.Height,
		bctx.BlockTime.UTC(),
	); err != nil {
		return fmt.Errorf("alpha_refine insert addr=%s: %w", p.PrimaryAddress, err)
	}
	return nil
}
