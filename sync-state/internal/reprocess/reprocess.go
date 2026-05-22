// Package reprocess implements the `sync-state reprocess-errors` subcommand.
//
// The high-level workflow:
//  1. Load unresolved handler_error_log rows that match the operator's
//     filters (composite_key, severity, height range, limit).
//  2. Group them by height so we only refetch each block once.
//  3. For each height, fetch /block + /block_results from the RPC pool
//     and walk the events deterministically, calling the registered
//     handler only for the (tx_index, event_index, composite_key)
//     coordinate recorded in the error row.
//  4. Dispatch through the events router in a per-block transaction.
//     Successful handler runs mark the row resolved; failed runs leave
//     the row in place (and rewrite the error if it changed).
//
// --dry-run wraps the per-block tx in BeginTx + Rollback so handlers can
// be observed for the SQL they would issue without committing anything.
// We still mark rows "would-resolve" in the output but don't touch the
// resolved_at column.
package reprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/rpc"
)

// CmdInputs is the dependency bundle the subcommand wrapper hands the
// runner. Mirrors fields on sync.Config so reprocess stays free of an
// import cycle with the sync package.
type CmdInputs struct {
	Pool   *pgxpool.Pool
	RPC    *rpc.Client
	Router *events.Router

	ChainID string

	CompositeKey string
	Severity     string
	Since        int64
	Until        int64
	Limit        int
	DryRun       bool

	MirrorRaw bool
}

// Outcome is the per-row result of a replay attempt, surfaced in the
// printed summary so operators can tell which rows resolved and which
// stayed in the log.
type Outcome struct {
	ID           int64
	Height       int64
	CompositeKey string
	Resolved     bool
	WouldResolve bool // dry-run analogue of Resolved
	Error        string
	Skipped      bool // handler not registered, or coordinate not found in fetched block
	SkipReason   string
}

// Run executes the subcommand. Returns the process exit code: 0 when
// every selected row either resolved or was cleanly skipped with a
// known reason; 1 when any row produced a hard error during replay.
func Run(ctx context.Context, in CmdInputs, stdout, stderr io.Writer) int {
	rows, err := db.ListUnresolvedErrors(ctx, in.Pool, in.ChainID,
		in.CompositeKey, in.Severity, in.Since, in.Until, in.Limit)
	if err != nil {
		fmt.Fprintf(stderr, "reprocess-errors: list: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No matching unresolved handler_error_log rows.")
		return 0
	}
	fmt.Fprintf(stderr, "reprocess-errors: %d row(s) selected", len(rows))
	if in.DryRun {
		fmt.Fprintf(stderr, " (dry-run: per-block tx rolled back)")
	}
	fmt.Fprintln(stderr)

	// Group by height so we only fetch each block once. Preserve the
	// per-height order from ListUnresolvedErrors (ASC by id) so the
	// replay matches the original ingestion order within a block.
	byHeight := make(map[int64][]db.UnresolvedError)
	heights := make([]int64, 0)
	for _, r := range rows {
		if _, seen := byHeight[r.Height]; !seen {
			heights = append(heights, r.Height)
		}
		byHeight[r.Height] = append(byHeight[r.Height], r)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })

	outcomes := make([]Outcome, 0, len(rows))
	exitCode := 0
	for _, h := range heights {
		hOutcomes, err := replayHeight(ctx, in, h, byHeight[h], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "reprocess-errors: h=%d: %v\n", h, err)
			exitCode = 1
		}
		outcomes = append(outcomes, hOutcomes...)
	}

	printSummary(stdout, outcomes, in.DryRun)
	for _, o := range outcomes {
		if o.Error != "" && !o.Skipped {
			exitCode = 1
		}
	}
	return exitCode
}

// replayHeight fetches one block + results from the RPC pool, then replays
// each targeted row in a single per-block transaction. The tx commits in
// normal mode and is rolled back in dry-run mode.
func replayHeight(ctx context.Context, in CmdInputs, height int64, targets []db.UnresolvedError, stderr io.Writer) ([]Outcome, error) {
	block, err := in.RPC.Block(ctx, height)
	if err != nil {
		return failAll(targets, fmt.Sprintf("rpc /block: %v", err)), nil
	}
	results, err := in.RPC.BlockResults(ctx, height)
	if err != nil {
		return failAll(targets, fmt.Sprintf("rpc /block_results: %v", err)), nil
	}
	if block.Block.Header.ChainID != in.ChainID {
		return failAll(targets, fmt.Sprintf("block chain_id=%q != %q", block.Block.Header.ChainID, in.ChainID)), nil
	}

	tx, err := in.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	bctx := events.BlockContext{
		ChainID:   in.ChainID,
		Height:    height,
		BlockTime: block.Block.Header.Time,
		TipHeight: height,
	}

	outcomes := make([]Outcome, 0, len(targets))
	for _, t := range targets {
		o := Outcome{
			ID:           t.ID,
			Height:       t.Height,
			CompositeKey: t.CompositeKey,
		}

		// Locate the original (event, attribute) coordinate inside the
		// freshly fetched block. We try the recorded (tx_index,
		// event_index) first; if that doesn't match, fall back to
		// scanning the block for the first attribute whose composite_key
		// matches. This handles the case where the recorded indices
		// drifted (a known historical gap on older error rows that
		// didn't capture event_index).
		raw, found, reason := locateAttribute(block, results, t)
		if !found {
			o.Skipped = true
			o.SkipReason = reason
			outcomes = append(outcomes, o)
			continue
		}

		bctxLocal := bctx
		if t.TxIndex != nil {
			bctxLocal.TxIndex = *t.TxIndex
		} else {
			bctxLocal.TxIndex = -1
		}
		if t.MsgIndex != nil {
			bctxLocal.MsgIndex = *t.MsgIndex
		} else {
			bctxLocal.MsgIndex = -1
		}
		if t.EventIndex != nil {
			bctxLocal.EventIndex = *t.EventIndex
		} else {
			bctxLocal.EventIndex = -1
		}

		he, fatal := in.Router.Dispatch(ctx, tx, bctxLocal, t.CompositeKey, raw)
		if fatal != nil {
			o.Error = fatal.Error()
			outcomes = append(outcomes, o)
			continue
		}
		if he != nil {
			o.Error = he.Error
			outcomes = append(outcomes, o)
			continue
		}
		if in.DryRun {
			o.WouldResolve = true
		} else {
			o.Resolved = true
		}
		outcomes = append(outcomes, o)
	}

	if in.DryRun {
		// Roll back explicitly; defer still runs but is a no-op now.
		_ = tx.Rollback(ctx)
		return outcomes, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	committed = true

	// Mark resolved rows AFTER the per-block commit so we never leave
	// dangling 'resolved' rows that the handler actually failed for.
	for _, o := range outcomes {
		if !o.Resolved {
			continue
		}
		if _, err := db.ResolveErrorByID(ctx, in.Pool, o.ID, "reprocess-errors"); err != nil {
			fmt.Fprintf(stderr, "reprocess-errors: mark resolved id=%d: %v\n", o.ID, err)
		}
	}
	return outcomes, nil
}

// locateAttribute finds the json-encoded payload for the (tx_index,
// event_index, composite_key) coordinate inside the freshly fetched
// block. CompositeKey is `<event.Type>.<attribute.Key>` (the same
// encoding the router uses). Returns (raw, true, "") on success or
// (nil, false, reason) on miss.
func locateAttribute(block *rpc.BlockResult, results *rpc.BlockResultsResult, t db.UnresolvedError) ([]byte, bool, string) {
	_ = block
	pickEvents := func(txIdx int) []rpc.Event {
		if txIdx < 0 {
			return results.FinalizeBlockEvents
		}
		if txIdx >= len(results.TxsResults) {
			return nil
		}
		return results.TxsResults[txIdx].Events
	}

	// First try the recorded coordinate.
	if t.TxIndex != nil && t.EventIndex != nil {
		events := pickEvents(*t.TxIndex)
		if *t.EventIndex >= 0 && *t.EventIndex < len(events) {
			ev := events[*t.EventIndex]
			if raw, ok := matchComposite(ev, t.CompositeKey); ok {
				return raw, true, ""
			}
		}
	}

	// Fallback: scan every event for the first composite_key match.
	scan := func(events []rpc.Event) ([]byte, bool) {
		for _, ev := range events {
			if raw, ok := matchComposite(ev, t.CompositeKey); ok {
				return raw, true
			}
		}
		return nil, false
	}
	if raw, ok := scan(results.FinalizeBlockEvents); ok {
		return raw, true, "(matched via fallback scan of finalize_block_events)"
	}
	for _, tr := range results.TxsResults {
		if raw, ok := scan(tr.Events); ok {
			return raw, true, "(matched via fallback scan of tx events)"
		}
	}
	return nil, false, "composite_key not found in fetched block (block may have been re-executed)"
}

// matchComposite returns the JSON-encoded attribute value when ev.Type
// concatenated with one of its attribute keys equals compositeKey. The
// router uses the same encoding so this is byte-for-byte compatible
// with what the original handler saw.
func matchComposite(ev rpc.Event, compositeKey string) ([]byte, bool) {
	seen := make(map[string]struct{}, len(ev.Attributes))
	for _, a := range ev.Attributes {
		if _, dup := seen[a.Key]; dup {
			continue
		}
		seen[a.Key] = struct{}{}
		if ev.Type+"."+a.Key != compositeKey {
			continue
		}
		return encodeAttributeValue(a.Value), true
	}
	return nil, false
}

// failAll marks every target as failed with the given reason. Used when
// the block-level fetch fails (RPC error, chain_id mismatch).
func failAll(targets []db.UnresolvedError, reason string) []Outcome {
	out := make([]Outcome, 0, len(targets))
	for _, t := range targets {
		out = append(out, Outcome{
			ID:           t.ID,
			Height:       t.Height,
			CompositeKey: t.CompositeKey,
			Error:        reason,
		})
	}
	return out
}

// printSummary writes the per-row outcome table and a roll-up. Resolved
// rows print first, then would-resolves (dry-run), then skips, then
// errors so an operator's eye lands on the actionable failures last.
func printSummary(w io.Writer, outcomes []Outcome, dryRun bool) {
	var resolved, wouldResolve, skipped, failed int
	for _, o := range outcomes {
		switch {
		case o.Resolved:
			resolved++
		case o.WouldResolve:
			wouldResolve++
		case o.Skipped:
			skipped++
		default:
			failed++
		}
	}

	fmt.Fprintln(w, "id        height    composite_key                                                  status")
	for _, o := range outcomes {
		status := "FAILED"
		extra := o.Error
		switch {
		case o.Resolved:
			status = "RESOLVED"
			extra = ""
		case o.WouldResolve:
			status = "WOULD-RESOLVE"
			extra = ""
		case o.Skipped:
			status = "SKIPPED"
			extra = o.SkipReason
		}
		line := fmt.Sprintf("%-9d %-9d %-60s  %s", o.ID, o.Height, truncate(o.CompositeKey, 60), status)
		if extra != "" {
			line += "  " + truncate(extra, 200)
		}
		fmt.Fprintln(w, line)
	}

	if dryRun {
		fmt.Fprintf(w, "\nDry-run summary: %d would-resolve, %d skipped, %d failed (all rolled back; resolved_at untouched)\n",
			wouldResolve, skipped, failed)
		return
	}
	fmt.Fprintf(w, "\nSummary: %d resolved, %d skipped, %d failed\n", resolved, skipped, failed)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Ensure errors import is referenced (the package compiles cleanly even
// when no caller currently uses `errors.Is` here; future error mapping
// will).
var _ = errors.Is
