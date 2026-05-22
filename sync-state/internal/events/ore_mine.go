package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// oreMineHandler ports cache.handle_event_ore_mine
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1990-2010).
//
// Single ledger row: credit to primaryAddress in `ore`, action=`mined`.
//
// Differences from the SQL handler (both intentional improvements):
//
//   - block_height is taken from bctx.Height. The SQL uses a sub-SELECT
//     from structs.current_block, which is racy across reorgs / replays
//     (current_block may have already advanced when the handler runs).
//     bctx.Height is the block the event lived in, period.
//
//   - time is taken from bctx.BlockTime instead of NOW(). The ledger is
//     a TimescaleDB hypertable partitioned on `time`; using BlockTime
//     means rows land in the chunk for the block they came from, which
//     is what time-series queries expect after a replay.
type oreMineHandler struct{}

func (oreMineHandler) CompositeKey() string {
	return "structs.structs.EventOreMine.eventOreMineDetail"
}

const oreMineInsertSQL = `
INSERT INTO structs.ledger (
    address, counterparty, amount_p, block_height, time, action, direction, denom
) VALUES ($1, NULL, $2, $3, $4, 'mined', 'credit', 'ore')`

func (oreMineHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.OreMine](raw)
	if err != nil {
		return err
	}
	if p.PrimaryAddress == "" {
		return fmt.Errorf("ore_mine: empty primaryAddress")
	}
	if _, err := tx.Exec(ctx, oreMineInsertSQL,
		p.PrimaryAddress,
		p.Amount.PgValue(),
		bctx.Height,
		bctx.BlockTime.UTC(),
	); err != nil {
		return fmt.Errorf("ore_mine insert addr=%s: %w", p.PrimaryAddress, err)
	}
	return nil
}
