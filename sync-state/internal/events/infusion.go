package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/payload"
)

// infusionHandler ports cache.handle_event_infusion
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:248-319).
// UPSERT on (destination_id, address). NO player_object sidecar.
//
// fuel/defusing/power/ratio are written to *_p (precision-preserving)
// columns; their non-_p companions are GENERATED in the schema and must
// not be touched here.
//
// When destination_type='struct' we also emit the ledger pair the
// dropped ADD_INFUSION_LEDGER_ENTRY trigger
// (trigger-infusion-ledger-entry.sql) used to write — see
// emitInfusionLedger below. The Go port fixes one behavioral wart in
// the SQL: it sourced block_height from structs.current_block (racy if
// processing out-of-order); we use bctx.Height + bctx.BlockTime for
// replay safety, same pattern as the Phase 5 ledger handlers.
type infusionHandler struct{}

func (infusionHandler) CompositeKey() string {
	return "structs.structs.EventInfusion.infusion"
}

const infusionUpsertSQL = `
INSERT INTO structs.infusion (
    destination_id, address, destination_type, player_id,
    fuel_p, defusing_p, power_p, ratio_p, commission,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
ON CONFLICT (destination_id, address) DO UPDATE
   SET fuel_p     = EXCLUDED.fuel_p,
       defusing_p = EXCLUDED.defusing_p,
       power_p    = EXCLUDED.power_p,
       ratio_p    = EXCLUDED.ratio_p,
       commission = EXCLUDED.commission,
       updated_at = NOW()
 WHERE structs.infusion.fuel_p     IS DISTINCT FROM EXCLUDED.fuel_p
    OR structs.infusion.defusing_p IS DISTINCT FROM EXCLUDED.defusing_p
    OR structs.infusion.power_p    IS DISTINCT FROM EXCLUDED.power_p
    OR structs.infusion.ratio_p    IS DISTINCT FROM EXCLUDED.ratio_p
    OR structs.infusion.commission IS DISTINCT FROM EXCLUDED.commission`

// infusionPrevFuelSelectSQL grabs the pre-upsert fuel_p so we can
// compute the delta the dropped SQL trigger needed on UPDATE
// (NEW.fuel_p - OLD.fuel_p). Only queried when destination_type='struct'
// (matches the trigger's outer gate). Stored as text so we go straight
// into big.Int without losing precision.
const infusionPrevFuelSelectSQL = `SELECT fuel_p::text FROM structs.infusion WHERE destination_id = $1 AND address = $2`

func (infusionHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Infusion](raw)
	if err != nil {
		return err
	}
	if p.DestinationID == "" || p.Address == "" {
		return fmt.Errorf("infusion: empty destination_id or address (dest=%q addr=%q)", p.DestinationID, p.Address)
	}

	// Capture pre-upsert state for the ledger derivation. Only the
	// destination_type='struct' branch needs it (the SQL outer gate).
	var (
		prevFuelKnown bool
		prevFuel      = new(big.Int)
	)
	if p.DestinationType == "struct" {
		var prevFuelStr *string
		err := tx.QueryRow(ctx, infusionPrevFuelSelectSQL, p.DestinationID, p.Address).Scan(&prevFuelStr)
		switch {
		case err == nil:
			prevFuelKnown = true
			if prevFuelStr != nil {
				if _, ok := prevFuel.SetString(*prevFuelStr, 10); !ok {
					return fmt.Errorf("infusion: prev fuel_p %q for (%s,%s) is not a numeric", *prevFuelStr, p.DestinationID, p.Address)
				}
			}
		case errors.Is(err, pgx.ErrNoRows):
			// fresh infusion; treated as INSERT path below
		default:
			return fmt.Errorf("infusion prev fuel (%s,%s): %w", p.DestinationID, p.Address, err)
		}
	}

	if _, err := tx.Exec(ctx, infusionUpsertSQL,
		p.DestinationID,
		p.Address,
		payload.NullableText(p.DestinationType),
		payload.NullableText(p.PlayerID),
		p.Fuel.PgValue(),
		p.Defusing.PgValue(),
		p.Power.PgValue(),
		p.Ratio.PgValue(),
		p.Commission.PgValue(),
	); err != nil {
		return fmt.Errorf("infusion upsert (%s, %s): %w", p.DestinationID, p.Address, err)
	}

	if p.DestinationType == "struct" {
		if err := emitInfusionLedger(ctx, tx, bctx, p, prevFuelKnown, prevFuel); err != nil {
			return fmt.Errorf("infusion ledger (%s,%s): %w", p.DestinationID, p.Address, err)
		}
	}
	return nil
}

// emitInfusionLedger ports structs.INFUSION_LEDGER_ENTRY
// (trigger-infusion-ledger-entry.sql:6-29).
//
//   - INSERT path  (no prev row): two ledger rows for the full fuel_p
//   - UPDATE path  (prev row):    two ledger rows for the DELTA, only
//     if NEW.fuel_p <> OLD.fuel_p
//
// Delta can be NEGATIVE (defusing). The SQL writes the raw signed delta;
// we do the same. Ledger amount_p is NUMERIC and accepts signed values.
//
// We use bctx.Height + bctx.BlockTime instead of structs.current_block
// + NOW() — the SQL trigger reads current_block which is racy with
// out-of-order processing; bctx is replay-safe and partitions
// correctly into the TimescaleDB hypertable.
func emitInfusionLedger(ctx context.Context, tx pgx.Tx, bctx BlockContext, p payload.Infusion, prevFuelKnown bool, prevFuel *big.Int) error {
	newFuel := new(big.Int)
	if s := p.Fuel.String(); s != "" {
		if _, ok := newFuel.SetString(s, 10); !ok {
			return fmt.Errorf("new fuel_p %q is not numeric", s)
		}
	}

	var amount *big.Int
	if !prevFuelKnown {
		// INSERT-equivalent: write the full fuel_p (SQL trigger does
		// the same — INSERT branch uses NEW.fuel_p directly).
		amount = newFuel
	} else {
		// UPDATE-equivalent: only emit if fuel_p actually changed.
		if newFuel.Cmp(prevFuel) == 0 {
			return nil
		}
		amount = new(big.Int).Sub(newFuel, prevFuel)
	}

	t := bctx.BlockTime.UTC()
	amtStr := amount.String()
	bctx.Buf.Ledger = append(bctx.Buf.Ledger,
		buffers.LedgerRow{Address: p.Address, Counterparty: p.DestinationID, AmountP: amtStr, BlockHeight: bctx.Height, Time: t, Action: "infused", Direction: "debit", Denom: "ualpha"},
		buffers.LedgerRow{Address: p.Address, Counterparty: p.DestinationID, AmountP: amtStr, BlockHeight: bctx.Height, Time: t, Action: "infused", Direction: "credit", Denom: "ualpha.infused"},
	)
	_ = tx
	return nil
}
