// Package verify implements the `sync-state verify` data-quality probe.
//
// Each check is a small, focused query that codifies one of the diagnostics
// an operator would have run manually after a fresh sync. Output is dual
// purpose: human-readable lines on stdout AND a row per result in
// sync_state.verification_report so the history is queryable.
//
// Adding a new check: add a CheckFunc to allChecks (or call it explicitly
// from Run for a check that needs custom inputs), return a CheckResult,
// and the runner takes care of persistence + formatting.
package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"sync-state/internal/db"
	"sync-state/internal/rpc"
)

// Status is the verdict of a single check.
type Status string

const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusInfo Status = "INFO" // known-acceptable observation (e.g. SQL-trigger artefacts)
	StatusSkip Status = "SKIP" // input unavailable (e.g. raw mirror off)
)

// CheckResult is the structured output of a single check. Counts is a
// free-form bag of supporting numbers (rendered as JSON in verification_report
// and dumped under each check in the text output).
type CheckResult struct {
	Name    string
	Status  Status
	Detail  string
	Counts  map[string]any
	Height  *int64 // optional context (e.g. "at height H")
	Started time.Time
	Elapsed time.Duration
}

// Inputs carries everything the runner needs to execute checks. Wired in
// from the subcommand. Tests construct this directly with a fixture-loaded
// pool and a nil rpc client to exercise individual checks.
type Inputs struct {
	Pool    db.Querier
	RPC     *rpc.Client // may be nil (skips checks that need a live tip)
	ChainID string

	// Tunables. Defaults are filled in by NewRunner if zero.
	LagWarn int64 // lag_blocks above which current_block_status is FAIL (default 5)

	// MirrorRaw mirrors the same flag on Config — when false, the
	// raw_mirror_coverage check is skipped instead of complaining about
	// an empty mirror table.
	MirrorRaw bool
}

// Options control which checks run and how the runner reports.
type Options struct {
	ErrorsOnly  bool // run only handler_errors_unresolved
	WriteReport bool // persist results to sync_state.verification_report (default true)
}

// runImpl executes every check, optionally persists results to
// sync_state.verification_report, and returns the rolled-up results.
// Errors are limited to infrastructure failures; individual check failures
// are reported as Status=FAIL inside the results slice. Exported via
// RunChecks in subcmd.go.
func runImpl(ctx context.Context, in Inputs, opts Options) ([]CheckResult, error) {
	if in.LagWarn <= 0 {
		in.LagWarn = 5
	}
	runID, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("generate run id: %w", err)
	}
	checks := selectChecks(opts)
	results := make([]CheckResult, 0, len(checks))
	for _, c := range checks {
		started := time.Now()
		r := c.fn(ctx, in)
		r.Name = c.name
		r.Started = started
		r.Elapsed = time.Since(started)
		results = append(results, r)
	}
	if opts.WriteReport {
		if err := writeReport(ctx, in.Pool, runID, results); err != nil {
			return results, fmt.Errorf("persist verification_report: %w", err)
		}
	}
	return results, nil
}

// checkEntry pairs a check name with its function for the runner.
type checkEntry struct {
	name string
	fn   CheckFunc
}

// CheckFunc is the signature every check implements.
type CheckFunc func(ctx context.Context, in Inputs) CheckResult

// allChecks is the registered check set, in the order the runner executes
// them. New checks append here.
var allChecks = []checkEntry{
	{"cursor_vs_tip", checkCursorVsTip},
	{"block_log_coverage", checkBlockLogCoverage},
	{"handler_errors_unresolved", checkHandlerErrorsUnresolved},
	{"planet_activity_seq_corruption", checkPlanetActivitySeqCorruption},
	{"block_height_nulls", checkBlockHeightNulls},
	{"raw_mirror_coverage", checkRawMirrorCoverage},
	{"current_block_status", checkCurrentBlockStatus},
	{"genesis_loaded", checkGenesisLoaded},
	{"ordered_timeseries_monotonic", checkOrderedTimeseriesMonotonic},
	{"ledger_balance_sanity", checkLedgerBalanceSanity},
}

func selectChecks(opts Options) []checkEntry {
	if opts.ErrorsOnly {
		for _, c := range allChecks {
			if c.name == "handler_errors_unresolved" {
				return []checkEntry{c}
			}
		}
	}
	return allChecks
}

// newRunID returns a UUID v4 string suitable for the
// sync_state.verification_report.run_id column. We synthesize one directly
// from crypto/rand so verify can stay free of external dependencies.
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// Set UUID v4 + RFC 4122 variant bits per RFC 4122 §4.4.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// writeReport persists one row per check to sync_state.verification_report.
// expected and actual are best-effort JSON: callers can leave them nil if
// the check is pass/fail with just a Detail line.
func writeReport(ctx context.Context, q db.Querier, runID string, results []CheckResult) error {
	for _, r := range results {
		var countsJSON []byte
		if len(r.Counts) > 0 {
			b, err := json.Marshal(r.Counts)
			if err != nil {
				return err
			}
			countsJSON = b
		}
		var height any
		if r.Height != nil {
			height = *r.Height
		}
		_, err := q.Exec(ctx, `
			INSERT INTO sync_state.verification_report
				(run_id, scope, height, composite_key, expected, actual, status, created_at)
			VALUES ($1, $2, $3, NULL, NULL, $4, $5, NOW())
		`, runID, r.Name, height, countsJSON, string(r.Status))
		if err != nil {
			return err
		}
	}
	return nil
}
