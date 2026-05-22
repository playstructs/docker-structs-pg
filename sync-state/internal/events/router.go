package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// SeverityError marks a HandlerError as a real failure: the handler aborted,
// the SAVEPOINT was rolled back, and the operator likely needs to inspect
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
	handlers map[string]Handler

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
		handlers:      make(map[string]Handler),
		unknownCounts: make(map[string]uint64),
		unknownDeltas: make(map[string]*UnknownDelta),
		StrictUnknown: strictUnknown,
	}
	for _, h := range AllHandlers() {
		key := h.CompositeKey()
		if _, exists := r.handlers[key]; exists {
			panic(fmt.Sprintf("duplicate handler for composite_key %q", key))
		}
		r.handlers[key] = h
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
// HandlerError is non-nil iff the handler failed; the block as a whole still
// commits (the error is written to sync_state.handler_error_log after the
// commit). This mirrors today's cache.add_queue EXCEPTION block behavior.
//
// Each handler runs inside its own SAVEPOINT so a failing PG statement only
// kills that handler's effect — the per-block tx remains usable for the
// remaining handlers, raw mirror, current_block, block_log, and cursor
// writes. Without this, one failure would cascade as SQLSTATE 25P02
// ("current transaction is aborted") through every subsequent write in
// the block.
func (r *Router) Dispatch(ctx context.Context, tx pgx.Tx, bctx BlockContext, compositeKey string, raw json.RawMessage) (he *HandlerError, fatal error) {
	h := r.Lookup(compositeKey)
	if h == nil {
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

	// SAVEPOINT around the handler so its failures don't poison the
	// outer per-block tx. pgx.Tx.Begin returns a nested transaction
	// implemented as SAVEPOINT (commit → RELEASE, rollback → ROLLBACK
	// TO SAVEPOINT). If the SAVEPOINT itself can't be created (e.g.
	// outer tx already aborted), fall back to direct dispatch on tx and
	// let the caller see the cascade.
	sp, spErr := tx.Begin(ctx)
	if spErr != nil {
		return &HandlerError{
			CompositeKey: compositeKey,
			Payload:      raw,
			Error:        fmt.Sprintf("savepoint begin: %v", spErr),
			Severity:     SeverityError,
			TxIndex:      ptrIfNotNeg(bctx.TxIndex),
			MsgIndex:     ptrIfNotNeg(bctx.MsgIndex),
			EventIndex:   ptrIfNotNeg(bctx.EventIndex),
		}, nil
	}

	// recover() so a panic in one handler doesn't abort the whole block;
	// the block still commits (other handlers + cursor + block_log), but
	// the event ends up in handler_error_log for replay.
	defer func() {
		if rec := recover(); rec != nil {
			_ = sp.Rollback(ctx)
			he = &HandlerError{
				CompositeKey: compositeKey,
				Payload:      raw,
				Error:        fmt.Sprintf("panic: %v", rec),
				Severity:     SeverityError,
				Stack:        string(debug.Stack()),
				TxIndex:      ptrIfNotNeg(bctx.TxIndex),
				MsgIndex:     ptrIfNotNeg(bctx.MsgIndex),
				EventIndex:   ptrIfNotNeg(bctx.EventIndex),
			}
		}
	}()

	if err := h.Handle(ctx, sp, bctx, raw); err != nil {
		// ErrSkipWithWarn = handler made a deterministic decision to
		// skip this event without dirtying the savepoint. Commit (=
		// RELEASE SAVEPOINT) so any incidental no-op writes the
		// handler did before deciding to skip still settle into the
		// outer tx, then emit a severity='warn' row for visibility.
		// We don't rollback because the contract is "no side effects"
		// — if the handler violated that, the writes are harmless and
		// the warn row records what happened either way.
		if errors.Is(err, ErrSkipWithWarn) {
			if cerr := sp.Commit(ctx); cerr != nil {
				// Commit-after-warn failure means the outer tx is in
				// trouble; surface as a real error rather than warn.
				return &HandlerError{
					CompositeKey: compositeKey,
					Payload:      raw,
					Error:        fmt.Sprintf("savepoint commit after warn: %v (original: %v)", cerr, err),
					Severity:     SeverityError,
					TxIndex:      ptrIfNotNeg(bctx.TxIndex),
					MsgIndex:     ptrIfNotNeg(bctx.MsgIndex),
					EventIndex:   ptrIfNotNeg(bctx.EventIndex),
				}, nil
			}
			return &HandlerError{
				CompositeKey: compositeKey,
				Payload:      raw,
				Error:        err.Error(),
				Severity:     SeverityWarn,
				TxIndex:      ptrIfNotNeg(bctx.TxIndex),
				MsgIndex:     ptrIfNotNeg(bctx.MsgIndex),
				EventIndex:   ptrIfNotNeg(bctx.EventIndex),
			}, nil
		}
		// Real error path: ROLLBACK TO SAVEPOINT — the outer tx
		// survives. Ignore rollback errors (handler error is more
		// interesting; outer tx may still be alive).
		_ = sp.Rollback(ctx)
		return &HandlerError{
			CompositeKey: compositeKey,
			Payload:      raw,
			Error:        err.Error(),
			Severity:     SeverityError,
			TxIndex:      ptrIfNotNeg(bctx.TxIndex),
			MsgIndex:     ptrIfNotNeg(bctx.MsgIndex),
			EventIndex:   ptrIfNotNeg(bctx.EventIndex),
		}, nil
	}
	if err := sp.Commit(ctx); err != nil {
		// RELEASE SAVEPOINT failed — handler's writes are lost; log
		// and continue. The outer tx is likely still alive.
		return &HandlerError{
			CompositeKey: compositeKey,
			Payload:      raw,
			Error:        fmt.Sprintf("savepoint commit: %v", err),
			Severity:     SeverityError,
			TxIndex:      ptrIfNotNeg(bctx.TxIndex),
			MsgIndex:     ptrIfNotNeg(bctx.MsgIndex),
			EventIndex:   ptrIfNotNeg(bctx.EventIndex),
		}, nil
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
	// Severity is SeverityError (real failure, rolled back) or
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
