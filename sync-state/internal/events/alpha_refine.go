package events

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
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
// The micro-unit multiplication was previously expressed in SQL as
// `$2::numeric * 1000000`. With the bulk buffer flush via pgx.CopyFrom
// there's no SQL parameter binding, so we do the multiplication in Go
// using math/big to keep arbitrary-precision NUMERIC math intact.
//
// See ore_mine.go for the bctx.Height / bctx.BlockTime rationale.
type alphaRefineHandler struct{}

func (alphaRefineHandler) CompositeKey() string {
	return "structs.structs.EventAlphaRefine.eventAlphaRefineDetail"
}

// alphaRefineMicroFactor matches the SQL `* 1000000::numeric` constant.
var alphaRefineMicroFactor = big.NewInt(1_000_000)

func (alphaRefineHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.AlphaRefine](raw)
	if err != nil {
		return err
	}
	if p.PrimaryAddress == "" {
		return fmt.Errorf("alpha_refine: empty primaryAddress")
	}
	amtRaw := p.Amount.String()
	amt := new(big.Int)
	if amtRaw == "" {
		return fmt.Errorf("alpha_refine: empty amount for addr=%s", p.PrimaryAddress)
	}
	if _, ok := amt.SetString(amtRaw, 10); !ok {
		return fmt.Errorf("alpha_refine: amount %q is not a numeric for addr=%s", amtRaw, p.PrimaryAddress)
	}
	microAlpha := new(big.Int).Mul(amt, alphaRefineMicroFactor).String()
	t := bctx.BlockTime.UTC()
	bctx.Buf.Ledger = append(bctx.Buf.Ledger,
		buffers.LedgerRow{Address: p.PrimaryAddress, AmountP: amtRaw, BlockHeight: bctx.Height, Time: t, Action: "refined", Direction: "debit", Denom: "ore"},
		buffers.LedgerRow{Address: p.PrimaryAddress, AmountP: microAlpha, BlockHeight: bctx.Height, Time: t, Action: "refined", Direction: "credit", Denom: "ualpha"},
	)
	_ = tx
	return nil
}
