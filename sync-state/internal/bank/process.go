package bank

import (
	"fmt"
	"time"

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
// Cross-event correlation is scoped to execution phase and message, with
// matching address/amount or staking operation evidence. Financial source
// coordinates remain the original unfiltered RPC ordinals.
func ProcessBlock(
	buf *buffers.Buffer,
	height int64,
	blockTime time.Time,
	chainID string,
	finalize []rpc.Event,
	txResults []rpc.TxResult,
) error {
	ordered, err := rpc.OrderedEvents(finalize, txResults)
	if err != nil {
		return err
	}
	type scope struct {
		phase   rpc.ExecutionPhase
		tx, msg int
	}
	groups := make(map[scope][]rpc.Event)
	indices := make([]int, len(ordered))
	for i, p := range ordered {
		key := scope{p.Phase, p.TxIndex, p.MsgIndex}
		indices[i] = len(groups[key])
		groups[key] = append(groups[key], p.Event)
	}
	for i, p := range ordered {
		if !IsBankEventType(p.Event.Type) {
			continue
		}
		key := scope{p.Phase, p.TxIndex, p.MsgIndex}
		id := eventIdent{ChainID: chainID, TxIndex: p.TxIndex, EventIndex: p.EventIndex, CorrelationIndex: indices[i]}
		if err := processEvent(buf, height, blockTime, id, p.Event, groups[key]); err != nil {
			return fmt.Errorf("phase=%d tx=%d event=%d %s: %w", p.Phase, p.TxIndex, p.EventIndex, p.Event.Type, err)
		}
	}
	return nil
}

// ProcessBuffer retains the original stream and coordinates, just like ingest.
func ProcessBuffer(buf *buffers.Buffer, blockTime time.Time, chainID string, evBuf EventBuffer) error {
	txs := make([]rpc.TxResult, len(evBuf.Txs))
	for i, evs := range evBuf.Txs {
		txs[i].Events = evs
	}
	return ProcessBlock(buf, evBuf.Height, blockTime, chainID, evBuf.Finalize, txs)
}

// Coin expansion never changes source coordinates or semantic leg numbers.
// Denomination is the extra dimension in the paired ledger uniqueness key.
func processEvent(buf *buffers.Buffer, height int64, t time.Time, id eventIdent, event rpc.Event, group []rpc.Event) error {
	coins, err := eventCoins(event)
	if err != nil {
		return err
	}
	if err := validateBankFields(event); err != nil {
		return err
	}
	for _, coin := range coins {
		ev := event
		ev.Attributes = append([]rpc.Attribute(nil), event.Attributes...)
		for i := range ev.Attributes {
			if ev.Attributes[i].Key == "amount" {
				ev.Attributes[i].Value = coin.amount + coin.denom
			}
		}
		switch ev.Type {
		case "transfer":
			err = handleTransfer(buf, height, t, id, ev, group)
		case "coinbase":
			err = handleCoinbase(buf, height, t, id, ev)
		case "burn":
			err = handleBurn(buf, height, t, id, ev)
		case "delegate":
			err = handleDelegate(buf, height, t, id, ev)
		case "redelegate":
			err = handleRedelegate(buf, height, t, id, ev, group)
		case "complete_redelegation":
			err = handleCompleteRedelegation(buf, height, t, id, ev)
		case "unbond":
			err = handleUnbond(buf, height, t, id, ev)
		case "cancel_unbond":
			err = handleCancelUnbond(buf, height, t, id, ev)
		case "complete_unbonding":
			err = handleCompleteUnbonding(buf, height, t, id, ev)
		case "create_validator":
			err = handleCreateValidator(buf, height, t, id, ev, group)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// eventIdent is the chain coordinate of the event producing ledger rows.
// MsgIndex is always -1 for bank events (they are not proto message
// indexes). EventIndex is the in-block event offset; legs are encoded
// via buffers.EncodeLedgerEventIndex.
type eventIdent struct {
	ChainID          string
	TxIndex          int
	EventIndex       int
	CorrelationIndex int // position within the same execution phase/message; never persisted
}

// --- buffer helpers ----------------------------------------------------

// pushLedger appends one structs.ledger row with the given counterparty
// and source-event identity. Use pushLedgerNoCp for actions without a
// counterparty (coinbase, burn).
func pushLedger(buf *buffers.Buffer, id eventIdent, leg int, address, counterparty, amount string, h int64, t time.Time, action, direction, denom string) {
	buf.Ledger = append(buf.Ledger, buffers.LedgerRow{
		Address:      address,
		Counterparty: counterparty,
		AmountP:      amount,
		BlockHeight:  h,
		Time:         t,
		Action:       action,
		Direction:    direction,
		Denom:        denom,
		ChainID:      id.ChainID,
		TxIndex:      id.TxIndex,
		MsgIndex:     -1,
		EventIndex:   buffers.EncodeLedgerEventIndex(id.EventIndex, leg),
	})
}

func pushLedgerNoCp(buf *buffers.Buffer, id eventIdent, leg int, address, amount string, h int64, t time.Time, action, direction, denom string) {
	pushLedger(buf, id, leg, address, "", amount, h, t, action, direction, denom)
}

// pushDefusion appends a structs.defusion row. completion is the raw
// chain "completion_time" attribute (RFC3339 with sub-second precision);
// we parse it once here so the buffered insert binds a real
// timestamptz value.
func pushDefusion(buf *buffers.Buffer, id eventIdent, height int64, sourceValidator, validatorAddr, delegatorAddr, defusionType, amount, denom, completion string, created time.Time) error {
	completed, err := parseChainTime(completion)
	if err != nil {
		return fmt.Errorf("defusion completed_at %q: %w", completion, err)
	}
	buf.Defusion = append(buf.Defusion, buffers.DefusionRow{
		ChainID:                id.ChainID,
		BlockHeight:            height,
		TxIndex:                id.TxIndex,
		EventIndex:             id.EventIndex,
		SourceValidatorAddress: sourceValidator,
		ValidatorAddress:       validatorAddr,
		DelegatorAddress:       delegatorAddr,
		DefusionType:           defusionType,
		AmountP:                amount,
		Denom:                  denom,
		CompletedAt:            completed,
		CreatedAt:              created,
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

// parseAmountDenom is used only after processEvent expands a validated coin set.
func parseAmountDenom(s string) (amount, denom string, ok bool) {
	coins, err := parseCoins(s)
	if err != nil || len(coins) != 1 {
		return "", "", false
	}
	return coins[0].amount, coins[0].denom, true
}

// findAttr returns the first attribute with key `k`, or "" if absent.
// Bank events tend to have unique keys per event so first-match is
// sufficient.
func findAttr(ev rpc.Event, k string) string {
	value, _ := lookupAttr(ev, k)
	return value
}

func lookupAttr(ev rpc.Event, k string) (string, bool) {
	for _, a := range ev.Attributes {
		if a.Key == k {
			return a.Value, true
		}
	}
	return "", false
}

// findAmount reads one normalized coin after processEvent has validated the
// required amount and expanded the full set. Empty sets never reach handlers.
func findAmount(ev rpc.Event) (amount, denom string, ok bool) {
	return parseAmountDenom(findAttr(ev, "amount"))
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
func handleTransfer(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	recipient := findAttr(ev, "recipient")
	sender := findAttr(ev, "sender")
	skipSent, skipReceived, err := transferOverlap(ev, group)
	if err != nil {
		return err
	}
	if skipSent {
		pushLedger(buf, id, 0, recipient, sender, amt, h, t, "received", "credit", denom)
		return nil
	}
	if skipReceived {
		pushLedger(buf, id, 0, sender, recipient, amt, h, t, "sent", "debit", denom)
		return nil
	}
	pushLedger(buf, id, 0, sender, recipient, amt, h, t, "sent", "debit", denom)
	pushLedger(buf, id, 1, recipient, sender, amt, h, t, "received", "credit", denom)
	return nil
}

// handleCoinbase ports the WHEN 'coinbase' branch (cache-system.sql:1454-1469).
// 1 ledger row: minter 'minted' credit, no counterparty.
func handleCoinbase(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	pushLedgerNoCp(buf, id, 0, findAttr(ev, "minter"), amt, h, t, "minted", "credit", denom)
	return nil
}

// handleBurn ports the WHEN 'burn' branch (cache-system.sql:1471-1486).
// 1 ledger row: burner 'burned' debit, no counterparty.
func handleBurn(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	pushLedgerNoCp(buf, id, 0, findAttr(ev, "burner"), amt, h, t, "burned", "debit", denom)
	return nil
}

// handleDelegate ports the WHEN 'delegate' branch (cache-system.sql:1488-1510).
// 3 ledger rows:
//
//  1. delegator (sender) debit '<denom>' (infused)
//  2. delegator credit '<denom>.infused'
//  3. validator (recipient) credit '<denom>.infused'
func handleDelegate(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	validator := findAttr(ev, "validator")
	delegator := findAttr(ev, "delegator")
	pushLedger(buf, id, 0, delegator, validator, amt, h, t, "infused", "debit", denom)
	pushLedger(buf, id, 1, delegator, validator, amt, h, t, "infused", "credit", denom+".infused")
	pushLedger(buf, id, 2, validator, delegator, amt, h, t, "infused", "credit", denom+".infused")
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
func handleRedelegate(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	src := findAttr(ev, "source_validator")
	dst := findAttr(ev, "destination_validator")
	delegator, err := redelegationOwner(group, id.CorrelationIndex, src)
	if err != nil {
		return err
	}
	completion := findAttr(ev, "completion_time")

	pushLedger(buf, id, 0, src, delegator, amt, h, t, "diversion_started", "debit", denom+".infused")
	pushLedger(buf, id, 1, delegator, src, amt, h, t, "diversion_started", "debit", denom+".infused")
	pushLedger(buf, id, 2, dst, delegator, amt, h, t, "diversion_started", "credit", denom+".defusing")
	pushLedger(buf, id, 3, delegator, dst, amt, h, t, "diversion_started", "credit", denom+".defusing")
	if completion != "" {
		if err := pushDefusion(buf, id, h, src, dst, delegator, "r", amt, denom, completion, t); err != nil {
			return err
		}
	}
	return nil
}

// handleCompleteRedelegation ports the WHEN 'complete_redelegation' branch
// (cache-system.sql:1557-1583). 4 ledger rows: 2 'diversion_completed' debits
// on '<denom>.defusing' + 2 'diversion_completed' credits on '<denom>.infused'.
func handleCompleteRedelegation(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	dst := findAttr(ev, "destination_validator")
	delegator := findAttr(ev, "delegator")
	pushLedger(buf, id, 0, dst, delegator, amt, h, t, "diversion_completed", "debit", denom+".defusing")
	pushLedger(buf, id, 1, delegator, dst, amt, h, t, "diversion_completed", "debit", denom+".defusing")
	pushLedger(buf, id, 2, dst, delegator, amt, h, t, "diversion_completed", "credit", denom+".infused")
	pushLedger(buf, id, 3, delegator, dst, amt, h, t, "diversion_completed", "credit", denom+".infused")
	return nil
}

// handleUnbond ports the WHEN 'unbond' branch (cache-system.sql:1585-1620).
// 4 ledger 'defusion_started' rows + 1 defusion row (type 'u').
//
//		sender    = delegator (user)
//		recipient = validator
//
//	 1. (val, deleg, debit, <denom>.infused)
//	 2. (deleg, val, debit, <denom>.infused)
//	 3. (val, deleg, credit, <denom>.defusing)
//	 4. (deleg, val, credit, <denom>.defusing)
func handleUnbond(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	completion := findAttr(ev, "completion_time")

	pushLedger(buf, id, 0, val, deleg, amt, h, t, "defusion_started", "debit", denom+".infused")
	pushLedger(buf, id, 1, deleg, val, amt, h, t, "defusion_started", "debit", denom+".infused")
	pushLedger(buf, id, 2, val, deleg, amt, h, t, "defusion_started", "credit", denom+".defusing")
	pushLedger(buf, id, 3, deleg, val, amt, h, t, "defusion_started", "credit", denom+".defusing")
	if completion != "" {
		if err := pushDefusion(buf, id, h, "", val, deleg, "u", amt, denom, completion, t); err != nil {
			return err
		}
	}
	return nil
}

// handleCancelUnbond ports the WHEN 'cancel_unbond' branch
// (cache-system.sql:1621-1645). 4 ledger 'defusion_cancelled' rows that
// reverse handleUnbond's first four.
func handleCancelUnbond(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	pushLedger(buf, id, 0, val, deleg, amt, h, t, "defusion_cancelled", "debit", denom+".defusing")
	pushLedger(buf, id, 1, deleg, val, amt, h, t, "defusion_cancelled", "debit", denom+".defusing")
	pushLedger(buf, id, 2, val, deleg, amt, h, t, "defusion_cancelled", "credit", denom+".infused")
	pushLedger(buf, id, 3, deleg, val, amt, h, t, "defusion_cancelled", "credit", denom+".infused")
	return nil
}

// handleCompleteUnbonding ports the WHEN 'complete_unbonding' branch
// (cache-system.sql:1646-1669). 3 ledger rows:
//
//  1. (val, deleg, debit, <denom>.defusing)
//  2. (deleg, val, debit, <denom>.defusing)
//  3. (deleg, val, credit, <denom>)   ← back to base denom (delegator's wallet)
func handleCompleteUnbonding(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	deleg := findAttr(ev, "delegator")
	pushLedger(buf, id, 0, val, deleg, amt, h, t, "defusion_completed", "debit", denom+".defusing")
	pushLedger(buf, id, 1, deleg, val, amt, h, t, "defusion_completed", "debit", denom+".defusing")
	pushLedger(buf, id, 2, deleg, val, amt, h, t, "defusion_completed", "credit", denom)
	return nil
}

// handleCreateValidator ports the WHEN 'create_validator' branch
// (cache-system.sql:1671-1699). 3 ledger rows for the initial self-delegation:
//
//		sender    = sibling coin_spent.spender (within same tx)
//		recipient = create_validator.validator
//
//	 1. (sender, val, debit, <denom>)
//	 2. (val, sender, credit, <denom>.infused)
//	 3. (sender, val, credit, <denom>.infused)
func handleCreateValidator(buf *buffers.Buffer, h int64, t time.Time, id eventIdent, ev rpc.Event, group []rpc.Event) error {
	amt, denom, ok := findAmount(ev)
	if !ok {
		return nil
	}
	val := findAttr(ev, "validator")
	spender, err := validatorSpender(group, amt, denom)
	if err != nil {
		return err
	}

	pushLedger(buf, id, 0, spender, val, amt, h, t, "infused", "debit", denom)
	pushLedger(buf, id, 1, val, spender, amt, h, t, "infused", "credit", denom+".infused")
	pushLedger(buf, id, 2, spender, val, amt, h, t, "infused", "credit", denom+".infused")
	return nil
}
