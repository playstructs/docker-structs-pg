package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// oreTheftHandler ports cache.handle_event_ore_theft
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2039-2064).
//
// Two ledger rows, different actions (no symmetric "stolen" action):
//   - thief:  action='seized',    direction='credit', counterparty=victim
//   - victim: action='forfeited', direction='debit',  counterparty=thief
//
// See ore_mine.go for the bctx.Height / bctx.BlockTime rationale.
type oreTheftHandler struct{}

func (oreTheftHandler) CompositeKey() string {
	return "structs.structs.EventOreTheft.eventOreTheftDetail"
}

const oreTheftInsertSQL = `
INSERT INTO structs.ledger (
    address, counterparty, amount_p, block_height, time, action, direction, denom
) VALUES
    ($1, $2, $3, $4, $5, 'seized',    'credit', 'ore'),
    ($2, $1, $3, $4, $5, 'forfeited', 'debit',  'ore')`

func (oreTheftHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.OreTheft](raw)
	if err != nil {
		return err
	}
	if p.ThiefPrimaryAddress == "" || p.VictimPrimaryAddress == "" {
		return fmt.Errorf("ore_theft: missing address (thief=%q victim=%q)", p.ThiefPrimaryAddress, p.VictimPrimaryAddress)
	}
	if _, err := tx.Exec(ctx, oreTheftInsertSQL,
		p.ThiefPrimaryAddress,
		p.VictimPrimaryAddress,
		p.Amount.PgValue(),
		bctx.Height,
		bctx.BlockTime.UTC(),
	); err != nil {
		return fmt.Errorf("ore_theft insert thief=%s victim=%s: %w", p.ThiefPrimaryAddress, p.VictimPrimaryAddress, err)
	}
	return nil
}
