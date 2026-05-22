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
// group (one tx, or the finalize_block batch) without any DB lookups.
package bank

import "sync-state/internal/rpc"

// bankEventTypes is the canonical IN list from cache.PROCESS_BLOCK_LEDGER.
// Used by IsBankEventType and by Capture for filtering.
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

// Capture is retained for backwards compatibility but is now a thin
// filter: it returns only the bank-relevant events from the block,
// preserving the (tx-indexed) grouping the ledger derivations need.
//
// New callers should hand the raw bundle directly to ProcessBlock; this
// is kept so callers that already hold an EventBuffer can keep using
// it.
func Capture(height int64, blockEvents []rpc.Event, txResults []rpc.TxResult) EventBuffer {
	var finalize []rpc.Event
	for _, e := range blockEvents {
		if IsBankEventType(e.Type) {
			finalize = append(finalize, e)
		}
	}
	txs := make([][]rpc.Event, len(txResults))
	for i, tr := range txResults {
		var g []rpc.Event
		for _, e := range tr.Events {
			if IsBankEventType(e.Type) {
				g = append(g, e)
			}
		}
		txs[i] = g
	}
	return EventBuffer{Height: height, Finalize: finalize, Txs: txs}
}

// EventBuffer holds a single block's bank-relevant events, grouped by tx
// so cross-event correlation (sibling withdraw_rewards / coin_spent) only
// searches within the same tx.
type EventBuffer struct {
	Height   int64
	Finalize []rpc.Event   // events with tx_id IS NULL in the SQL world
	Txs      [][]rpc.Event // per-tx event lists (tx_index = slice index)
}
