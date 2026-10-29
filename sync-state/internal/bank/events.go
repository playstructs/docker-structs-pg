// Package bank fans cosmos-sdk bank/staking events (transfer, coinbase,
// burn, delegate, redelegate, complete_redelegation, unbond, cancel_unbond,
// complete_unbonding, create_validator) out into structs.ledger and
// structs.defusion. This is the Go port of cache.PROCESS_BLOCK_LEDGER /
// cache.TRANSFER_LEDGER_ENTRY (cache-system.sql:1399-1704).
//
// Architecture difference from the SQL:
//
//   - The SQL pipeline ran PROCESS_BLOCK_LEDGER for block N-1 when block N
//     was inserted into cache.blocks (a 1-block lag), because update-cache
//     populated cache.events/cache.attributes only after each block was
//     persisted. sync-state has the events for block N in memory before
//     applyBlock commits, so we process bank events for the CURRENT block
//     inline inside the per-block transaction. No lag, no carry-forward
//     state.
//
//   - We use bctx.Height + bctx.BlockTime for block_height/time on every
//     ledger row (the SQL used _block.height + NOW(), the latter of which
//     skews TimescaleDB chunk placement on replay).
//
// Cross-event correlation (redelegate↔withdraw_rewards.delegator;
// create_validator↔coin_spent.spender) is resolved within the same event
// execution phase/message and matched operation without any DB lookups.
package bank

import "sync-state/internal/rpc"

// bankEventTypes is the canonical IN list from cache.PROCESS_BLOCK_LEDGER.
// Used by IsBankEventType during complete-stream processing.
var bankEventTypes = map[string]struct{}{
	"transfer":              {},
	"coinbase":              {},
	"burn":                  {},
	"delegate":              {},
	"redelegate":            {},
	"complete_redelegation": {},
	"unbond":                {},
	"cancel_unbond":         {},
	"complete_unbonding":    {},
	"create_validator":      {},
}

// IsBankEventType returns true if the given event type is one of the 10
// types ProcessBlock handles. Exposed so callers can pre-filter without
// poking at the unexported map.
func IsBankEventType(t string) bool {
	_, ok := bankEventTypes[t]
	return ok
}

// Capture retains every event: filtering would renumber financial identities
// and discard message/receipt context needed to correlate accounting legs.
func Capture(height int64, blockEvents []rpc.Event, txResults []rpc.TxResult) EventBuffer {
	txs := make([][]rpc.Event, len(txResults))
	for i, tr := range txResults {
		txs[i] = tr.Events
	}
	return EventBuffer{Height: height, Finalize: blockEvents, Txs: txs}
}

// EventBuffer holds a single block's original events and coordinates.
// ProcessBlock establishes the execution-phase/message correlation scopes.
type EventBuffer struct {
	Height   int64
	Finalize []rpc.Event   // events with tx_id IS NULL in the SQL world
	Txs      [][]rpc.Event // per-tx event lists (tx_index = slice index)
}
