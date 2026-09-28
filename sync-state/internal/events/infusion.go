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
// structsd v0.21.0 re-homes infusion.playerId on address reassignment
// while keeping the (destination_id, address) key. The conflict UPDATE
// must write player_id or the upgrade-block repair is silently dropped.
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
   SET player_id  = EXCLUDED.player_id,
       fuel_p     = EXCLUDED.fuel_p,
       defusing_p = EXCLUDED.defusing_p,
       power_p    = EXCLUDED.power_p,
       ratio_p    = EXCLUDED.ratio_p,
       commission = EXCLUDED.commission,
       updated_at = NOW()
 WHERE structs.infusion.player_id  IS DISTINCT FROM EXCLUDED.player_id
    OR structs.infusion.fuel_p     IS DISTINCT FROM EXCLUDED.fuel_p
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

// EventInfusion is a snapshot (fuel, not the amount infused), so the
// ledger delta is only correct against the row state immediately before
// this event. sync_state.infusion_event_position records the last event
// applied to each row; anything at or before it (a replay over existing
// state, or reprocess-errors retrying an old event after newer ones
// landed) is skipped, because diffing it against newer state writes a
// wrong, often negative, delta.
const infusionPositionSelectSQL = `
SELECT height, tx_index, event_index
  FROM sync_state.infusion_event_position
 WHERE destination_id = $1 AND address = $2`

// Rows written before infusion_event_position existed have no position.
// The last 'infused' ledger pair for the row bounds its state to that
// block; the intra-block position is unknown, so the whole block counts
// as applied. NULL when there is no infusion row at all.
const infusionLegacyHeightSelectSQL = `
SELECT CASE WHEN i.destination_id IS NULL THEN NULL ELSE COALESCE((
         SELECT max(l.block_height)
           FROM structs.ledger l
          WHERE l.address = $1 AND l.counterparty = $2 AND l.action = 'infused'), 0)
       END
  FROM (SELECT 1) one
  LEFT JOIN structs.infusion i ON i.destination_id = $2 AND i.address = $1`

const infusionPositionUpsertSQL = `
INSERT INTO sync_state.infusion_event_position (destination_id, address, height, tx_index, event_index)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (destination_id, address) DO UPDATE
   SET height      = EXCLUDED.height,
       tx_index    = EXCLUDED.tx_index,
       event_index = EXCLUDED.event_index`

// infusionEventApplied reports whether the row already reflects an event
// at or after bctx's position. Positions follow dispatch order: height,
// then tx_index (-1 for finalize_block events), then event_index.
func infusionEventApplied(ctx context.Context, tx pgx.Tx, bctx BlockContext, p payload.Infusion) (bool, error) {
	var h int64
	var txIdx, evIdx int
	err := tx.QueryRow(ctx, infusionPositionSelectSQL, p.DestinationID, p.Address).Scan(&h, &txIdx, &evIdx)
	switch {
	case err == nil:
		if bctx.Height != h {
			return bctx.Height < h, nil
		}
		if bctx.TxIndex != txIdx {
			return bctx.TxIndex < txIdx, nil
		}
		return bctx.EventIndex <= evIdx, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("infusion position (%s,%s): %w", p.DestinationID, p.Address, err)
	}

	var legacy *int64
	if err := tx.QueryRow(ctx, infusionLegacyHeightSelectSQL, p.Address, p.DestinationID).Scan(&legacy); err != nil {
		return false, fmt.Errorf("infusion legacy height (%s,%s): %w", p.DestinationID, p.Address, err)
	}
	return legacy != nil && bctx.Height <= *legacy, nil
}

func (infusionHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Infusion](raw)
	if err != nil {
		return err
	}
	if p.DestinationID == "" || p.Address == "" {
		return fmt.Errorf("infusion: empty destination_id or address (dest=%q addr=%q)", p.DestinationID, p.Address)
	}

	applied, err := infusionEventApplied(ctx, tx, bctx, p)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}
	if _, err := tx.Exec(ctx, infusionPositionUpsertSQL,
		p.DestinationID, p.Address, bctx.Height, bctx.TxIndex, bctx.EventIndex,
	); err != nil {
		return fmt.Errorf("infusion position upsert (%s,%s): %w", p.DestinationID, p.Address, err)
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
	if p.DestinationType == "reactor" {
		bctx.Dirty.Reactor(p.DestinationID)
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

	amtStr := amount.String()
	appendLedger(bctx,
		buffers.LedgerRow{Address: p.Address, Counterparty: p.DestinationID, AmountP: amtStr, Action: "infused", Direction: "debit", Denom: "ualpha"},
		buffers.LedgerRow{Address: p.Address, Counterparty: p.DestinationID, AmountP: amtStr, Action: "infused", Direction: "credit", Denom: "ualpha.infused"},
	)
	_ = tx
	return nil
}
