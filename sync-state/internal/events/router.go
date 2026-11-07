package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// SeverityError marks a HandlerError as a real failure: the handler aborted,
// its transaction must be rolled back, and the operator likely needs to inspect
// and/or replay via `sync-state reprocess-errors`. This is the default.
const SeverityError = "error"

// SeverityWarn marks a HandlerError as a recoverable skip: the handler
// chose to no-op rather than fail (typically by returning ErrSkipWithWarn),
// no SQL was rolled back, and the row exists primarily for operator
// visibility. The startup banner and `sync-state verify` surface these
// separately from real errors.
const SeverityWarn = "warn"

// Router dispatches a composite_key + payload to its registered Handler.
// Built once at startup from AllHandlers(); thread-safe for concurrent
// dispatch (handlers receive their own tx, so the router itself just
// reads-only into its map).
type Router struct {
	handlers         map[string]Handler
	nestedAttributes map[string][]string

	// Unknown-event accounting. Mutated under unknownMu.
	//
	// Two maps are maintained:
	//   - unknownCounts is the cumulative-since-process-start tally
	//     used by the periodic stderr summary. Reset via
	//     ResetUnknownSummary() after each log emission.
	//   - unknownDeltas is the never-reset-by-log map used by the
	//     persistence path (syncer flushes deltas to
	//     sync_state.unknown_event_log after each window and clears
	//     this map via DrainUnknownDeltas).
	// Both increment in lock-step under unknownMu.
	unknownMu     sync.Mutex
	unknownCounts map[string]uint64
	unknownDeltas map[string]*UnknownDelta
	unknownTotal  atomic.Uint64

	// StrictUnknown=true means an unknown composite_key is a fatal error;
	// false (default) means it gets counted and skipped.
	StrictUnknown bool
}

// UnknownDelta is the per-composite_key accumulator that persists to
// sync_state.unknown_event_log. SamplePayload holds the *latest* raw
// attribute value seen for the key — enough for an operator to pivot
// into sync_state.raw_attributes for full samples.
type UnknownDelta struct {
	Count         uint64
	FirstHeight   int64
	LastHeight    int64
	SamplePayload json.RawMessage
}

// NewRouter builds the dispatch table from AllHandlers(). Panics if two
// handlers claim the same composite_key — that's a programming error and
// caught at startup.
func NewRouter(strictUnknown bool) *Router {
	r := &Router{
		handlers:         make(map[string]Handler),
		nestedAttributes: make(map[string][]string),
		unknownCounts:    make(map[string]uint64),
		unknownDeltas:    make(map[string]*UnknownDelta),
		StrictUnknown:    strictUnknown,
	}
	for _, h := range AllHandlers() {
		key := h.CompositeKey()
		if _, exists := r.handlers[key]; exists {
			panic(fmt.Sprintf("duplicate handler for composite_key %q", key))
		}
		r.handlers[key] = h
		if event, attribute, nested := strings.Cut(strings.TrimPrefix(key, "structs.structs."), "."); nested {
			typeName := "structs.structs." + event
			r.nestedAttributes[typeName] = append(r.nestedAttributes[typeName], attribute)
		}
	}
	return r
}

// Lookup returns the handler for compositeKey, or nil if none registered.
func (r *Router) Lookup(compositeKey string) Handler {
	return r.handlers[compositeKey]
}

// Handlers returns a snapshot of the registered handlers, sorted by
// composite_key. Used by `sync-state list-handlers`.
func (r *Router) Handlers() []Handler {
	out := make([]Handler, 0, len(r.handlers))
	for _, h := range r.handlers {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CompositeKey() < out[j].CompositeKey()
	})
	return out
}

// Count returns the number of registered handlers.
func (r *Router) Count() int { return len(r.handlers) }

// Dispatch routes one event through the registry. Returns:
//   - nil if the handler ran successfully (or the key was unknown and not strict),
//   - a HandlerError describing what to log if the handler returned an error or panicked,
//   - a fatal error if StrictUnknown is true and the key is unknown.
//
// HandlerError is non-nil iff the handler failed or deliberately skipped.
// The ingestion caller aborts the block/window on SeverityError and persists
// diagnostics outside that transaction. Explicit legacy warnings may continue.
//
// Dispatch isolates each handler with a SAVEPOINT so callers such as error
// reprocessing can recover from failures and continue using the transaction.
func (r *Router) Dispatch(ctx context.Context, tx pgx.Tx, bctx BlockContext, compositeKey string, raw json.RawMessage) (*HandlerError, error) {
	return r.dispatch(ctx, tx, bctx, compositeKey, raw, true)
}

// DispatchAtomic runs a handler directly in the caller's block/window
// transaction. On a fatal error or any HandlerError other than SeverityWarn,
// the caller MUST stop dispatching, roll back the transaction, and discard
// its buffers and dirty sets. No per-handler savepoint or snapshot is made.
func (r *Router) DispatchAtomic(ctx context.Context, tx pgx.Tx, bctx BlockContext, compositeKey string, raw json.RawMessage) (*HandlerError, error) {
	return r.dispatch(ctx, tx, bctx, compositeKey, raw, false)
}

func (r *Router) dispatch(ctx context.Context, tx pgx.Tx, bctx BlockContext, compositeKey string, raw json.RawMessage, isolated bool) (he *HandlerError, fatal error) {
	h := r.Lookup(compositeKey)
	if h == nil {
		if IgnoreUnknown(compositeKey) {
			return nil, nil
		}
		r.unknownMu.Lock()
		r.unknownCounts[compositeKey]++
		d, ok := r.unknownDeltas[compositeKey]
		if !ok {
			d = &UnknownDelta{FirstHeight: bctx.Height, LastHeight: bctx.Height}
			r.unknownDeltas[compositeKey] = d
		}
		d.Count++
		if bctx.Height < d.FirstHeight {
			d.FirstHeight = bctx.Height
		}
		if bctx.Height > d.LastHeight {
			d.LastHeight = bctx.Height
		}
		// Latest sample wins; cheap & enough context for ops to pivot
		// into sync_state.raw_attributes if they want every payload.
		d.SamplePayload = raw
		r.unknownMu.Unlock()
		r.unknownTotal.Add(1)
		if r.StrictUnknown {
			return nil, fmt.Errorf("strict-unknown-events: no handler for %q at height %d", compositeKey, bctx.Height)
		}
		return nil, nil
	}

	failure := func(err error, severity string) *HandlerError {
		return &HandlerError{
			CompositeKey: compositeKey, Payload: raw, Error: err.Error(), Severity: severity,
			TxIndex: ptrIfNotNeg(bctx.TxIndex), MsgIndex: ptrIfNotNeg(bctx.MsgIndex),
			EventIndex: ptrIfNotNeg(bctx.EventIndex),
		}
	}

	workTx := tx
	rollback := func() {}
	commit := func() error { return nil }
	if isolated {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return failure(fmt.Errorf("savepoint begin: %w", err), SeverityError), nil
		}
		workTx = sp
		bufSnap := bctx.Buf.Snapshot()
		dirtySnap := bctx.Dirty.Snapshot()
		restore := func() {
			bctx.Buf.Restore(bufSnap)
			bctx.Dirty.Restore(dirtySnap)
		}
		rollback = func() {
			_ = sp.Rollback(ctx)
			restore()
		}
		commit = func() error {
			err := sp.Commit(ctx)
			if err != nil {
				restore()
			}
			return err
		}
	}

	defer func() {
		if rec := recover(); rec != nil {
			rollback()
			he = failure(fmt.Errorf("panic: %v", rec), SeverityError)
			he.Stack = string(debug.Stack())
		}
	}()

	err := h.Handle(ctx, workTx, bctx, raw)
	// Releasing a savepoint used to expose SQL failures even when a handler
	// swallowed the error or mislabeled it as a warning. Preserve that check
	// without sending another query on the atomic ingestion path.
	if !isolated && (err == nil || errors.Is(err, ErrSkipWithWarn)) && tx.Conn().PgConn().TxStatus() == 'E' {
		return failure(fmt.Errorf("handler left transaction aborted (result: %v)", err), SeverityError), nil
	}
	if err != nil {
		if errors.Is(err, ErrSkipWithWarn) {
			if cerr := commit(); cerr != nil {
				return failure(fmt.Errorf("savepoint commit after warn: %w (original: %v)", cerr, err), SeverityError), nil
			}
			return failure(err, SeverityWarn), nil
		}
		rollback()
		return failure(err, SeverityError), nil
	}
	if err := commit(); err != nil {
		return failure(fmt.Errorf("savepoint commit: %w", err), SeverityError), nil
	}
	return nil, nil
}

// HandlerError is the router's view of a per-event failure (or recoverable
// warn). The sync block writer translates this into a
// sync_state.handler_error_log row.
type HandlerError struct {
	CompositeKey string
	Payload      json.RawMessage
	Error        string
	// Severity is SeverityError (real failure requiring rollback) or
	// SeverityWarn (handler returned ErrSkipWithWarn, no rollback).
	// Defaults to SeverityError when empty so older callers that don't
	// set it explicitly still get the safer label.
	Severity   string
	Stack      string
	TxIndex    *int
	MsgIndex   *int
	EventIndex *int
}

// UnknownSummary returns a snapshot of unknown-composite-key counts since
// last reset. Used by the periodic summary logger.
func (r *Router) UnknownSummary() (total uint64, byKey map[string]uint64) {
	r.unknownMu.Lock()
	defer r.unknownMu.Unlock()
	out := make(map[string]uint64, len(r.unknownCounts))
	for k, v := range r.unknownCounts {
		out[k] = v
	}
	return r.unknownTotal.Load(), out
}

// ResetUnknownSummary clears the periodic-log counters. Call after the
// stderr summary so the next interval reports fresh top-N. Does NOT
// touch unknownDeltas — those keep accumulating until DrainUnknownDeltas
// runs the persistence flush.
func (r *Router) ResetUnknownSummary() {
	r.unknownMu.Lock()
	r.unknownCounts = make(map[string]uint64)
	r.unknownMu.Unlock()
	r.unknownTotal.Store(0)
}

// DrainUnknownDeltas atomically returns the accumulated per-key deltas
// since the last call and clears the in-memory map. Caller is expected
// to persist these into sync_state.unknown_event_log so a periodic
// flush survives restarts and operators can query the complete picture.
//
// Returns the snapshot by value (callers can iterate without holding
// the router's lock).
func (r *Router) DrainUnknownDeltas() map[string]UnknownDelta {
	r.unknownMu.Lock()
	defer r.unknownMu.Unlock()
	if len(r.unknownDeltas) == 0 {
		return nil
	}
	out := make(map[string]UnknownDelta, len(r.unknownDeltas))
	for k, v := range r.unknownDeltas {
		out[k] = *v
	}
	r.unknownDeltas = make(map[string]*UnknownDelta)
	return out
}

func ptrIfNotNeg(v int) *int {
	if v < 0 {
		return nil
	}
	return &v
}

// IgnoreUnknown reports composite keys that must never reach
// sync_state.unknown_event_log. Cosmos SDK distribution emits
// commission.* and rewards.* on every block (~5 attributes, millions of
// row updates); cosmos.* / ibc.* modules are similarly unhandled by
// design. structs.* keys are never ignored.
func IgnoreUnknown(compositeKey string) bool {
	switch {
	case compositeKey == "commission", compositeKey == "rewards":
		return true
	case strings.HasPrefix(compositeKey, "commission."),
		strings.HasPrefix(compositeKey, "rewards."),
		strings.HasPrefix(compositeKey, "cosmos."),
		strings.HasPrefix(compositeKey, "ibc."):
		return true
	}
	return false
}
