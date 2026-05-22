// Package events defines the Handler interface that every chain-event handler
// implements, plus the BlockContext that carries per-block state into each
// handler call. Concrete handlers (one file per composite_key) live alongside
// this file; see registry.go for the complete list.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
)

// Handler is implemented by one struct per chain event sync-state knows how
// to process. Implementations live in this package as one file per event.
//
// See the plan section "Code organization in sync-state/" for the contract
// rules: handlers are pure functions of (BlockContext, payload, tx); they
// must be replay-safe (prefer payload data over DB lookups for any value
// that's available in the event); they must use parameterized SQL ($n) or
// pgx.CopyFrom with compile-time identifiers — never construct identifiers
// from chain-derived strings.
type Handler interface {
	// CompositeKey is "<event_type>.<attribute_key>", the same string
	// cache.attributes.composite_key holds in the existing pipeline.
	// E.g. "structs.structs.EventAttack.eventAttackDetail".
	CompositeKey() string

	// Handle runs inside the per-block transaction. raw is the JSON payload
	// (the chain attribute's value, JSON-decoded into the right type by the
	// router via the payload package). Returning an error logs to
	// sync_state.handler_error_log and the per-block tx continues with the
	// next event (the block as a whole still commits — see Error model in
	// the plan).
	Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error
}

// BlockContext carries everything a handler may need from the block being
// processed. Helpers like NextPlanetActivitySeq are wired by the router at
// runtime so handlers don't need to import internal/db.
//
// Note: the previous SYNC_STATE_OWN_DERIVATIONS / OWN_INFUSION_LEDGER /
// OWN_PLANET_META flags lived here while sync-state was being introduced
// alongside the legacy cache.* triggers. They were removed once the
// cache subsystem was retired — every handler now unconditionally owns
// its derivation side-effects (planet_activity emits, planet_meta seed,
// infusion ledger, address-guild propagation).
type BlockContext struct {
	ChainID   string
	Height    int64
	BlockTime time.Time
	TipHeight int64

	// Per-event coordinates (filled by the router):
	// TxIndex < 0 means a finalize_block (BeginBlock/EndBlock) event.
	TxIndex    int
	MsgIndex   int
	EventIndex int

	// Helpers populated by the router. May be nil in tests that exercise
	// handlers in isolation.
	NextPlanetActivitySeq   func(ctx context.Context, tx pgx.Tx, planetID string) (int64, error)
	ResolveActivityLocation func(ctx context.Context, tx pgx.Tx, structID string) (string, error)

	// Buf is the per-block (streaming) or per-window (bulk) append-only
	// row buffer. Handlers that write to ledger / defusion /
	// planet_activity / stat_* push rows here; the orchestrator
	// pgx.CopyFrom-flushes them before commit. nil-safe so tests that
	// drive handlers in isolation don't need to construct one.
	Buf *buffers.Buffer
}
