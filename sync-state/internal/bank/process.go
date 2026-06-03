package bank

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/rpc"
)

// ProcessBlock is the Go port of cache.PROCESS_BLOCK_LEDGER. It walks the
// finalize_block events and each tx's events in order, pushing
// structs.ledger / structs.defusion rows for the 10 bank/staking event
// types into buf. Rows are flushed by the orchestrator via
// buf.Flush(ctx, tx) at end-of-block / end-of-window.
//
// All rows use height/blockTime instead of NOW(), matching the
// ledger-handler convention from Phase 5 (replay-safe + correct
// hypertable partitioning).
//
// Cross-event correlation (redelegate/create_validator) only looks within
// the same event group — same tx for tx-bound events, or the finalize
// batch for finalize events. This mirrors the SQL's
// `events.tx_id = event.tx_id` join.
//
// tx is kept on the signature for symmetry with the legacy code path and
// to leave room for future bank handlers that need direct query access;
// today only the buffer is touched.
func ProcessBlock(
	ctx context.Context,
	tx pgx.Tx,
	buf *buffers.Buffer,
	height int64,
	blockTime time.Time,
	finalize []rpc.Event,
	txResults []rpc.TxResult,
) error {
	if err := processGroup(ctx, tx, buf, height, blockTime, finalize); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	for i, tr := range txResults {
		if err := processGroup(ctx, tx, buf, height, blockTime, tr.Events); err != nil {
			return fmt.Errorf("tx[%d]: %w", i, err)
		}
	}
	return nil
}

// ProcessBuffer is a convenience wrapper for callers that already hold an
// EventBuffer (e.g. the legacy Capture path). New callers should prefer
// ProcessBlock so they don't pay the extra Capture filtering pass.
func ProcessBuffer(ctx context.Context, tx pgx.Tx, buf *buffers.Buffer, blockTime time.Time, evBuf EventBuffer) error {
	if err := processGroup(ctx, tx, buf, evBuf.Height, blockTime, evBuf.Finalize); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	for i, evs := range evBuf.Txs {
		if err := processGroup(ctx, tx, buf, evBuf.Height, blockTime, evs); err != nil {
			return fmt.Errorf("tx[%d]: %w", i, err)
		}
	}
	return nil
}

// processGroup walks every bank-relevant event in `events` and pushes
// rows into buf, passing the same slice as the cross-event lookup pool.
// Non-bank events are skipped silently.
func processGroup(ctx context.Context, tx pgx.Tx, buf *buffers.Buffer, height int64, t time.Time, events []rpc.Event) error {
	for _, ev := range events {
		if !IsBankEventType(ev.Type) {
			continue
		}
		var err error
		switch ev.Type {
		case "transfer":
			err = handleTransfer(buf, height, t, ev, events)
		case "coinbase":
			err = handleCoinbase(buf, height, t, ev)
		case "burn":
			err = handleBurn(buf, height, t, ev)
		case "delegate":
			err = handleDelegate(buf, height, t, ev)
		case "redelegate":
			err = handleRedelegate(buf, height, t, ev, events)
		case "complete_redelegation":
			err = handleCompleteRedelegation(buf, height, t, ev)
		case "unbond":
			err = handleUnbond(buf, height, t, ev)
		case "cancel_unbond":
			err = handleCancelUnbond(buf, height, t, ev)
		case "complete_unbonding":
			err = handleCompleteUnbonding(buf, height, t, ev)
		case "create_validator":
			err = handleCreateValidator(buf, height, t, ev, events)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", ev.Type, err)
		}
	}
	_ = ctx
	_ = tx
	return nil
}

// --- buffer helpers ----------------------------------------------------

// pushLedger appends one structs.ledger row with the given counterparty.
// Use pushLedgerNoCp for actions without a counterparty (coinbase, burn).
func pushLedger(buf *buffers.Buffer, address, counterparty, amount string, h int64, t time.Time, action, direction, denom string) {
	buf.Ledger = append(buf.Ledger, buffers.LedgerRow{
		Address:      address,
		Counterparty: counterparty,
		AmountP:      amount,
		BlockHeight:  h,
		Time:         t,
		Action:       action,
		Direction:    direction,
		Denom:        denom,
	})
}

func pushLedgerNoCp(buf *buffers.Buffer, address, amount string, h int64, t time.Time, action, direction, denom string) {
	pushLedger(buf, address, "", amount, h, t, action, direction, denom)
}

// pushDefusion appends a structs.defusion row. completion is the raw
// chain "completion_time" attribute (RFC3339 with sub-second precision);
// we parse it once here so the buffered CopyFrom emits a real
// timestamptz value.
func pushDefusion(buf *buffers.Buffer, validatorAddr, delegatorAddr, defusionType, amount, denom, completion string, created time.Time) error {
	completed, err := parseChainTime(completion)
	if err != nil {
		return fmt.Errorf("defusion completed_at %q: %w", completion, err)
	}
	buf.Defusion = append(buf.Defusion, buffers.DefusionRow{
		ValidatorAddress: validatorAddr,
		DelegatorAddress: delegatorAddr,
		DefusionType:     defusionType,
		AmountP:          amount,
		Denom:            denom,
		CompletedAt:      completed,
		CreatedAt:        created,
	})
	return nil
}

// parseChainTime accepts either RFC3339Nano (the canonical Cosmos format
// for completion_time) or RFC3339. Returns the value in UTC.
func parseChainTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

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

// isStructInfusionTx reports whether the event group is a struct-generator
// infusion tx. The infusion handler already debits the player via 'infused';
// the companion Cosmos transfer (player → module escrow → burn) must not
// also write a duplicate 'sent' on the player. Reactor infusions use
// destinationType=reactor and delegate/unbond bank events instead.
func isStructInfusionTx(group []rpc.Event) bool {
	for _, e := range group {
		if e.Type != "structs.structs.EventInfusion" {
			continue
		}
		infusion := findAttr(e, "infusion")
		if infusion != "" && strings.Contains(infusion, `"destinationType":"struct"`) {
			return true
		}
	}
	for _, e := range group {
		if e.Type == "message" && findAttr(e, "action") == "/structs.structs.MsgStructGeneratorInfuse" {
			return true
		}
	}
	return false
}

// isAlphaRefineTx reports whether the event group is an ore-refinery complete
// tx. The alphaRefine handler already credits the player via 'refined';
// the companion Cosmos transfer (pool → player) must not also write a
// duplicate 'received' on the player.
func isAlphaRefineTx(group []rpc.Event) bool {
	for _, e := range group {
		if e.Type == "structs.structs.EventAlphaRefine" {
			return true
		}
	}
	for _, e := range group {
		if e.Type == "message" && findAttr(e, "action") == "/structs.structs.MsgStructOreRefineryComplete" {
			return true
		}
	}
	return false
}

// handleTransfer ports the WHEN 'transfer' branch (cache-system.sql:1433-1452).
// 2 ledger rows: sender 'sent' debit + recipient 'received' credit.
//
// Struct infusion txs are an exception: emitInfusionLedger already records
// the player debit, so skip the sender 'sent' row but keep 'received' on the
// escrow pool so received+burned still net to zero there.
//
// Alpha refine txs are the mirror case: alphaRefineHandler already credits
// the player via 'refined', so skip the recipient 'received' row but keep
// 'sent' on the pool so minted+sent still net correctly there.
func handleTransfer(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	recipient := findAttr(ev, "recipient")
	sender := findAttr(ev, "sender")
	if isStructInfusionTx(group) {
		pushLedger(buf, recipient, sender, amt, h, t, "received", "credit", denom)
		return nil
	}
	if isAlphaRefineTx(group) {
		pushLedger(buf, sender, recipient, amt, h, t, "sent", "debit", denom)
		return nil
	}
	pushLedger(buf, sender, recipient, amt, h, t, "sent", "debit", denom)
	pushLedger(buf, recipient, sender, amt, h, t, "received", "credit", denom)
	return nil
}

// handleCoinbase ports the WHEN 'coinbase' branch (cache-system.sql:1454-1469).
// 1 ledger row: minter 'minted' credit, no counterparty.
func handleCoinbase(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	pushLedgerNoCp(buf, findAttr(ev, "minter"), amt, h, t, "minted", "credit", denom)
	return nil
}

// handleBurn ports the WHEN 'burn' branch (cache-system.sql:1471-1486).
// 1 ledger row: burner 'burned' debit, no counterparty.
func handleBurn(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	pushLedgerNoCp(buf, findAttr(ev, "burner"), amt, h, t, "burned", "debit", denom)
	return nil
}

// handleDelegate ports the WHEN 'delegate' branch (cache-system.sql:1488-1510).
// 3 ledger rows:
//
//  1. delegator (sender) debit '<denom>' (infused)
//  2. delegator credit '<denom>.infused'
//  3. validator (recipient) credit '<denom>.infused'
func handleDelegate(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	validator := findAttr(ev, "validator")
	delegator := findAttr(ev, "delegator")
	pushLedger(buf, delegator, validator, amt, h, t, "infused", "debit", denom)
	pushLedger(buf, delegator, validator, amt, h, t, "infused", "credit", denom+".infused")
	pushLedger(buf, validator, delegator, amt, h, t, "infused", "credit", denom+".infused")
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
func handleRedelegate(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	src := findAttr(ev, "source_validator")
	dst := findAttr(ev, "destination_validator")
	delegator := findInGroup(group, "withdraw_rewards", "delegator")
	completion := findAttr(ev, "completion_time")

	pushLedger(buf, src, delegator, amt, h, t, "diversion_started", "debit", denom+".infused")
	pushLedger(buf, delegator, src, amt, h, t, "diversion_started", "debit", denom+".infused")
	pushLedger(buf, dst, delegator, amt, h, t, "diversion_started", "credit", denom+".defusing")
	pushLedger(buf, delegator, dst, amt, h, t, "diversion_started", "credit", denom+".defusing")
	if completion != "" {
		if err := pushDefusion(buf, dst, delegator, "r", amt, denom, completion, t); err != nil {
			return err
		}
	}
	return nil
}

// handleCompleteRedelegation ports the WHEN 'complete_redelegation' branch
// (cache-system.sql:1557-1583). 4 ledger rows: 2 'diversion_completed' debits
// on '<denom>.defusing' + 2 'diversion_completed' credits on '<denom>.infused'.
func handleCompleteRedelegation(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	dst := findAttr(ev, "destination_validator")
	delegator := findAttr(ev, "delegator")
	pushLedger(buf, dst, delegator, amt, h, t, "diversion_completed", "debit", denom+".defusing")
	pushLedger(buf, delegator, dst, amt, h, t, "diversion_completed", "debit", denom+".defusing")
	pushLedger(buf, dst, delegator, amt, h, t, "diversion_completed", "credit", denom+".infused")
	pushLedger(buf, delegator, dst, amt, h, t, "diversion_completed", "credit", denom+".infused")
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
func handleUnbond(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	completion := findAttr(ev, "completion_time")

	pushLedger(buf, val, deleg, amt, h, t, "defusion_started", "debit", denom+".infused")
	pushLedger(buf, deleg, val, amt, h, t, "defusion_started", "debit", denom+".infused")
	pushLedger(buf, val, deleg, amt, h, t, "defusion_started", "credit", denom+".defusing")
	pushLedger(buf, deleg, val, amt, h, t, "defusion_started", "credit", denom+".defusing")
	if completion != "" {
		if err := pushDefusion(buf, val, deleg, "u", amt, denom, completion, t); err != nil {
			return err
		}
	}
	return nil
}

// handleCancelUnbond ports the WHEN 'cancel_unbond' branch
// (cache-system.sql:1621-1645). 4 ledger 'defusion_cancelled' rows that
// reverse handleUnbond's first four.
func handleCancelUnbond(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	pushLedger(buf, val, deleg, amt, h, t, "defusion_cancelled", "debit", denom+".defusing")
	pushLedger(buf, deleg, val, amt, h, t, "defusion_cancelled", "debit", denom+".defusing")
	pushLedger(buf, val, deleg, amt, h, t, "defusion_cancelled", "credit", denom+".infused")
	pushLedger(buf, deleg, val, amt, h, t, "defusion_cancelled", "credit", denom+".infused")
	return nil
}

// handleCompleteUnbonding ports the WHEN 'complete_unbonding' branch
// (cache-system.sql:1646-1669). 3 ledger rows:
//
//  1. (val, deleg, debit, <denom>.defusing)
//  2. (deleg, val, debit, <denom>.defusing)
//  3. (deleg, val, credit, <denom>)   ← back to base denom (delegator's wallet)
func handleCompleteUnbonding(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	pushLedger(buf, val, deleg, amt, h, t, "defusion_completed", "debit", denom+".defusing")
	pushLedger(buf, deleg, val, amt, h, t, "defusion_completed", "debit", denom+".defusing")
	pushLedger(buf, deleg, val, amt, h, t, "defusion_completed", "credit", denom)
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
func handleCreateValidator(buf *buffers.Buffer, h int64, t time.Time, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	spender := findInGroup(group, "coin_spent", "spender")

	pushLedger(buf, spender, val, amt, h, t, "infused", "debit", denom)
	pushLedger(buf, val, spender, amt, h, t, "infused", "credit", denom+".infused")
	pushLedger(buf, spender, val, amt, h, t, "infused", "credit", denom+".infused")
	return nil
}
