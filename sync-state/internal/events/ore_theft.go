package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
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

func (oreTheftHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.OreTheft](raw)
	if err != nil {
		return err
	}
	if p.ThiefPrimaryAddress == "" || p.VictimPrimaryAddress == "" {
		return fmt.Errorf("ore_theft: missing address (thief=%q victim=%q)", p.ThiefPrimaryAddress, p.VictimPrimaryAddress)
	}
	amt := p.Amount.String()
	t := bctx.BlockTime.UTC()
	bctx.Buf.Ledger = append(bctx.Buf.Ledger,
		buffers.LedgerRow{Address: p.ThiefPrimaryAddress, Counterparty: p.VictimPrimaryAddress, AmountP: amt, BlockHeight: bctx.Height, Time: t, Action: "seized", Direction: "credit", Denom: "ore"},
		buffers.LedgerRow{Address: p.VictimPrimaryAddress, Counterparty: p.ThiefPrimaryAddress, AmountP: amt, BlockHeight: bctx.Height, Time: t, Action: "forfeited", Direction: "debit", Denom: "ore"},
	)
	_ = tx
	return nil
}
