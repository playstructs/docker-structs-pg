package bank

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/rpc"
)

// ProcessBlock is the Go port of cache.PROCESS_BLOCK_LEDGER. It walks the
// finalize_block events and each tx's events in order, writing
// structs.ledger / structs.defusion rows for the 10 bank/staking event
// types.
//
// All rows use height/blockTime instead of NOW(), matching the
// ledger-handler convention from Phase 5 (replay-safe + correct
// hypertable partitioning).
//
// Cross-event correlation (redelegate/create_validator) only looks within
// the same event group — same tx for tx-bound events, or the finalize
// batch for finalize events. This mirrors the SQL's
// `events.tx_id = event.tx_id` join.
func ProcessBlock(
	ctx context.Context,
	tx pgx.Tx,
	height int64,
	blockTime time.Time,
	finalize []rpc.Event,
	txResults []rpc.TxResult,
) error {
	if err := processGroup(ctx, tx, height, blockTime, finalize); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	for i, tr := range txResults {
		if err := processGroup(ctx, tx, height, blockTime, tr.Events); err != nil {
			return fmt.Errorf("tx[%d]: %w", i, err)
		}
	}
	return nil
}

// ProcessBuffer is a convenience wrapper for callers that already hold an
// EventBuffer (e.g. the legacy Capture path). New callers should prefer
// ProcessBlock so they don't pay the extra Capture filtering pass.
func ProcessBuffer(ctx context.Context, tx pgx.Tx, blockTime time.Time, buf EventBuffer) error {
	if err := processGroup(ctx, tx, buf.Height, blockTime, buf.Finalize); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	for i, evs := range buf.Txs {
		if err := processGroup(ctx, tx, buf.Height, blockTime, evs); err != nil {
			return fmt.Errorf("tx[%d]: %w", i, err)
		}
	}
	return nil
}

// processGroup runs every bank-relevant event in `events` against tx,
// passing the same slice as the cross-event lookup pool. Non-bank events
// are skipped silently.
func processGroup(ctx context.Context, tx pgx.Tx, height int64, t time.Time, events []rpc.Event) error {
	for _, ev := range events {
		if !IsBankEventType(ev.Type) {
			continue
		}
		var err error
		switch ev.Type {
		case "transfer":
			err = handleTransfer(ctx, tx, height, t, ev)
		case "coinbase":
			err = handleCoinbase(ctx, tx, height, t, ev)
		case "burn":
			err = handleBurn(ctx, tx, height, t, ev)
		case "delegate":
			err = handleDelegate(ctx, tx, height, t, ev)
		case "redelegate":
			err = handleRedelegate(ctx, tx, height, t, ev, events)
		case "complete_redelegation":
			err = handleCompleteRedelegation(ctx, tx, height, t, ev)
		case "unbond":
			err = handleUnbond(ctx, tx, height, t, ev)
		case "cancel_unbond":
			err = handleCancelUnbond(ctx, tx, height, t, ev)
		case "complete_unbonding":
			err = handleCompleteUnbonding(ctx, tx, height, t, ev)
		case "create_validator":
			err = handleCreateValidator(ctx, tx, height, t, ev, events)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", ev.Type, err)
		}
	}
	return nil
}

// --- shared SQL --------------------------------------------------------

const insertLedgerSQL = `
INSERT INTO structs.ledger (
    address, counterparty, amount_p, block_height, time, action, direction, denom
) VALUES ($1, $2, $3::numeric, $4, $5, $6, $7, $8)`

// insertLedgerNoCpSQL is used for actions that have no counterparty
// (coinbase, burn). Mirrors the SQL where the INSERT omits counterparty.
const insertLedgerNoCpSQL = `
INSERT INTO structs.ledger (
    address, amount_p, block_height, time, action, direction, denom
) VALUES ($1, $2::numeric, $3, $4, $5, $6, $7)`

const insertDefusionSQL = `
INSERT INTO structs.defusion (
    validator_address, delegator_address, defusion_type,
    amount_p, denom, completed_at, created_at
) VALUES ($1, $2, $3, $4::numeric, $5, $6::timestamptz, $7)`

// --- attribute helpers -------------------------------------------------

// amountDenomRegex parses "123ualpha" / "0.5uvert" into the amount and
// denom parts. Matches the SQL `(^[0-9\.]+)([a-zA-Z0-9\.-]+)` regex.
var amountDenomRegex = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([a-zA-Z0-9.\-]+)`)

// parseAmountDenom returns ("123","ualpha",true) for "123ualpha", or
// ("","",false) when the input is empty or doesn't match. Multi-coin
// values like "100ualpha,500uvert" only return the first coin (mirrors
// regexp_matches which returns the first match).
func parseAmountDenom(s string) (amount, denom string, ok bool) {
	if s == "" {
		return "", "", false
	}
	m := amountDenomRegex.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// findAttr returns the first attribute with key `k`, or "" if absent.
// Bank events tend to have unique keys per event so first-match is
// sufficient.
func findAttr(ev rpc.Event, k string) string {
	for _, a := range ev.Attributes {
		if a.Key == k {
			return a.Value
		}
	}
	return ""
}

// findAmount parses the event's "amount" attribute into amount/denom.
// Returns ok=false when missing, empty, or malformed — handlers treat
// that as "skip this event" (the SQL did the same via amount_populated).
func findAmount(ev rpc.Event) (amount, denom string, ok bool) {
	return parseAmountDenom(findAttr(ev, "amount"))
}

// findInGroup returns the first occurrence of attribute `attrKey` on
// any event of type `eventType` within group. Mirrors the SQL's
// IN (SELECT events.rowid ... WHERE type=$1 AND tx_id = event.tx_id)
// sub-select for redelegate/create_validator cross-references.
func findInGroup(group []rpc.Event, eventType, attrKey string) string {
	for _, e := range group {
		if e.Type != eventType {
			continue
		}
		if v := findAttr(e, attrKey); v != "" {
			return v
		}
	}
	return ""
}

// --- per-event handlers ------------------------------------------------

// handleTransfer ports the WHEN 'transfer' branch (cache-system.sql:1433-1452).
// 2 ledger rows: sender 'sent' debit + recipient 'received' credit.
func handleTransfer(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	recipient := findAttr(ev, "recipient")
	sender := findAttr(ev, "sender")
	if _, err := tx.Exec(ctx, insertLedgerSQL, sender, recipient, amt, h, t, "sent", "debit", denom); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, recipient, sender, amt, h, t, "received", "credit", denom); err != nil {
		return err
	}
	return nil
}

// handleCoinbase ports the WHEN 'coinbase' branch (cache-system.sql:1454-1469).
// 1 ledger row: minter 'minted' credit, no counterparty.
func handleCoinbase(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	minter := findAttr(ev, "minter")
	_, err := tx.Exec(ctx, insertLedgerNoCpSQL, minter, amt, h, t, "minted", "credit", denom)
	return err
}

// handleBurn ports the WHEN 'burn' branch (cache-system.sql:1471-1486).
// 1 ledger row: burner 'burned' debit, no counterparty.
func handleBurn(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	burner := findAttr(ev, "burner")
	_, err := tx.Exec(ctx, insertLedgerNoCpSQL, burner, amt, h, t, "burned", "debit", denom)
	return err
}

// handleDelegate ports the WHEN 'delegate' branch (cache-system.sql:1488-1510).
// 3 ledger rows:
//
//  1. delegator (sender) debit '<denom>' (infused)
//  2. delegator credit '<denom>.infused'
//  3. validator (recipient) credit '<denom>.infused'
func handleDelegate(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	validator := findAttr(ev, "validator")
	delegator := findAttr(ev, "delegator")
	if _, err := tx.Exec(ctx, insertLedgerSQL, delegator, validator, amt, h, t, "infused", "debit", denom); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, delegator, validator, amt, h, t, "infused", "credit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, validator, delegator, amt, h, t, "infused", "credit", denom+".infused"); err != nil {
		return err
	}
	return nil
}

// handleRedelegate ports the WHEN 'redelegate' branch (cache-system.sql:1513-1556).
// 4 ledger rows + 1 defusion row.
//
//	sender    = source_validator       (losing delegation)
//	recipient = destination_validator  (gaining delegation)
//	delegator = sibling withdraw_rewards.delegator (within same tx)
//
// All four rows are 'diversion_started':
//  1. (src_val, delegator, debit, <denom>.infused)
//  2. (delegator, src_val, debit, <denom>.infused)
//  3. (dst_val, delegator, credit, <denom>.defusing)
//  4. (delegator, dst_val, credit, <denom>.defusing)
//
// defusion row: (dst_val, delegator, 'r', amount, denom, completion_time, NOW()).
func handleRedelegate(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	src := findAttr(ev, "source_validator")
	dst := findAttr(ev, "destination_validator")
	delegator := findInGroup(group, "withdraw_rewards", "delegator")
	completion := findAttr(ev, "completion_time")

	if _, err := tx.Exec(ctx, insertLedgerSQL, src, delegator, amt, h, t, "diversion_started", "debit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, delegator, src, amt, h, t, "diversion_started", "debit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, dst, delegator, amt, h, t, "diversion_started", "credit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, delegator, dst, amt, h, t, "diversion_started", "credit", denom+".defusing"); err != nil {
		return err
	}
	if completion != "" {
		if _, err := tx.Exec(ctx, insertDefusionSQL, dst, delegator, "r", amt, denom, completion, t); err != nil {
			return err
		}
	}
	return nil
}

// handleCompleteRedelegation ports the WHEN 'complete_redelegation' branch
// (cache-system.sql:1557-1583). 4 ledger rows: 2 'diversion_completed' debits
// on '<denom>.defusing' + 2 'diversion_completed' credits on '<denom>.infused'.
func handleCompleteRedelegation(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	dst := findAttr(ev, "destination_validator")
	delegator := findAttr(ev, "delegator")
	if _, err := tx.Exec(ctx, insertLedgerSQL, dst, delegator, amt, h, t, "diversion_completed", "debit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, delegator, dst, amt, h, t, "diversion_completed", "debit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, dst, delegator, amt, h, t, "diversion_completed", "credit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, delegator, dst, amt, h, t, "diversion_completed", "credit", denom+".infused"); err != nil {
		return err
	}
	return nil
}

// handleUnbond ports the WHEN 'unbond' branch (cache-system.sql:1585-1620).
// 4 ledger 'defusion_started' rows + 1 defusion row (type 'u').
//
//	sender    = delegator (user)
//	recipient = validator
//
//  1. (val, deleg, debit, <denom>.infused)
//  2. (deleg, val, debit, <denom>.infused)
//  3. (val, deleg, credit, <denom>.defusing)
//  4. (deleg, val, credit, <denom>.defusing)
func handleUnbond(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	completion := findAttr(ev, "completion_time")

	if _, err := tx.Exec(ctx, insertLedgerSQL, val, deleg, amt, h, t, "defusion_started", "debit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, deleg, val, amt, h, t, "defusion_started", "debit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, val, deleg, amt, h, t, "defusion_started", "credit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, deleg, val, amt, h, t, "defusion_started", "credit", denom+".defusing"); err != nil {
		return err
	}
	if completion != "" {
		if _, err := tx.Exec(ctx, insertDefusionSQL, val, deleg, "u", amt, denom, completion, t); err != nil {
			return err
		}
	}
	return nil
}

// handleCancelUnbond ports the WHEN 'cancel_unbond' branch
// (cache-system.sql:1621-1645). 4 ledger 'defusion_cancelled' rows that
// reverse handleUnbond's first four.
func handleCancelUnbond(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	if _, err := tx.Exec(ctx, insertLedgerSQL, val, deleg, amt, h, t, "defusion_cancelled", "debit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, deleg, val, amt, h, t, "defusion_cancelled", "debit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, val, deleg, amt, h, t, "defusion_cancelled", "credit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, deleg, val, amt, h, t, "defusion_cancelled", "credit", denom+".infused"); err != nil {
		return err
	}
	return nil
}

// handleCompleteUnbonding ports the WHEN 'complete_unbonding' branch
// (cache-system.sql:1646-1669). 3 ledger rows:
//
//  1. (val, deleg, debit, <denom>.defusing)
//  2. (deleg, val, debit, <denom>.defusing)
//  3. (deleg, val, credit, <denom>)   ← back to base denom (delegator's wallet)
func handleCompleteUnbonding(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	if _, err := tx.Exec(ctx, insertLedgerSQL, val, deleg, amt, h, t, "defusion_completed", "debit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, deleg, val, amt, h, t, "defusion_completed", "debit", denom+".defusing"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, deleg, val, amt, h, t, "defusion_completed", "credit", denom); err != nil {
		return err
	}
	return nil
}

// handleCreateValidator ports the WHEN 'create_validator' branch
// (cache-system.sql:1671-1699). 3 ledger rows for the initial self-delegation:
//
//	sender    = sibling coin_spent.spender (within same tx)
//	recipient = create_validator.validator
//
//  1. (sender, val, debit, <denom>)
//  2. (val, sender, credit, <denom>.infused)
//  3. (sender, val, credit, <denom>.infused)
func handleCreateValidator(ctx context.Context, tx pgx.Tx, h int64, t time.Time, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	spender := findInGroup(group, "coin_spent", "spender")

	if _, err := tx.Exec(ctx, insertLedgerSQL, spender, val, amt, h, t, "infused", "debit", denom); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, val, spender, amt, h, t, "infused", "credit", denom+".infused"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertLedgerSQL, spender, val, amt, h, t, "infused", "credit", denom+".infused"); err != nil {
		return err
	}
	return nil
}
