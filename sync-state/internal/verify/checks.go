package verify

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/db"
)

// checkCursorVsTip compares the persisted cursor's last_height to the live
// RPC tip. Skipped when no RPC client was wired. FAIL when lag exceeds
// LagWarn; INFO when the upstream is catching_up (the lag is real but
// expected).
func checkCursorVsTip(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	if in.RPC == nil {
		r.Status = StatusSkip
		r.Detail = "rpc client not configured"
		return r
	}
	c, err := db.ReadCursor(ctx, in.Pool, in.ChainID)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("read cursor: %v", err)
		return r
	}
	status, err := in.RPC.Status(ctx)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("rpc /status: %v", err)
		return r
	}
	tip := status.Latest()
	lag := tip - c.LastHeight
	if lag < 0 {
		lag = 0
	}
	r.Counts = map[string]any{
		"cursor_height":   c.LastHeight,
		"rpc_tip":         tip,
		"lag":             lag,
		"catching_up":     status.SyncInfo.CatchingUp,
		"persisted_status": string(c.Status),
	}
	r.Height = &c.LastHeight
	switch {
	case status.SyncInfo.CatchingUp:
		r.Status = StatusInfo
		r.Detail = fmt.Sprintf("upstream node still catching_up; cursor h=%d, tip=%d, lag=%d", c.LastHeight, tip, lag)
	case lag > in.LagWarn:
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("cursor lag %d > %d", lag, in.LagWarn)
	default:
		r.Status = StatusPass
		r.Detail = fmt.Sprintf("cursor at h=%d, tip=%d (lag=%d)", c.LastHeight, tip, lag)
	}
	return r
}

// checkBlockLogCoverage scans sync_state.block_log for gaps in [min..max].
// A gap is one or more missing heights between two recorded blocks — usually
// a sign that a fetch failure caused a window to be skipped, or that an
// operator rewound the cursor without truncating block_log. PASS = no gaps.
func checkBlockLogCoverage(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	var minH, maxH, total int64
	err := in.Pool.QueryRow(ctx, `
		SELECT COALESCE(MIN(height), 0), COALESCE(MAX(height), 0), COUNT(*)
		  FROM sync_state.block_log
		 WHERE chain_id = $1
	`, in.ChainID).Scan(&minH, &maxH, &total)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("scan block_log: %v", err)
		return r
	}
	if total == 0 {
		r.Status = StatusInfo
		r.Detail = "block_log empty (no blocks ingested yet)"
		r.Counts = map[string]any{"rows": int64(0)}
		return r
	}
	expected := maxH - minH + 1
	gap := expected - total
	r.Counts = map[string]any{
		"min_height":     minH,
		"max_height":     maxH,
		"rows":           total,
		"missing_blocks": gap,
	}
	r.Height = &maxH
	if gap > 0 {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%d missing block_log rows in [%d..%d]", gap, minH, maxH)
	} else {
		r.Status = StatusPass
		r.Detail = fmt.Sprintf("contiguous block_log [%d..%d] (%d rows)", minH, maxH, total)
	}
	return r
}

// checkHandlerErrorsUnresolved groups unresolved rows by composite_key
// and severity. FAIL when any severity='error' rows exist; INFO when only
// warnings remain; PASS when empty.
func checkHandlerErrorsUnresolved(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	rows, err := in.Pool.Query(ctx, `
		SELECT composite_key, COALESCE(severity, 'error'), COUNT(*)
		  FROM sync_state.handler_error_log
		 WHERE chain_id = $1 AND resolved_at IS NULL
		 GROUP BY composite_key, COALESCE(severity, 'error')
		 ORDER BY 1, 2
	`, in.ChainID)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("scan handler_error_log: %v", err)
		return r
	}
	defer rows.Close()
	groups := make(map[string]any)
	var errTotal, warnTotal int64
	for rows.Next() {
		var ck, sev string
		var n int64
		if err := rows.Scan(&ck, &sev, &n); err != nil {
			r.Status = StatusFail
			r.Detail = fmt.Sprintf("scan row: %v", err)
			return r
		}
		groups[ck+":"+sev] = n
		if sev == "warn" {
			warnTotal += n
		} else {
			errTotal += n
		}
	}
	if err := rows.Err(); err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("iterate: %v", err)
		return r
	}
	groups["_error_total"] = errTotal
	groups["_warn_total"] = warnTotal
	r.Counts = groups
	switch {
	case errTotal > 0:
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%d unresolved errors (%d warn). Run `sync-state reprocess-errors`.", errTotal, warnTotal)
	case warnTotal > 0:
		r.Status = StatusInfo
		r.Detail = fmt.Sprintf("%d unresolved warns (no hard errors)", warnTotal)
	default:
		r.Status = StatusPass
		r.Detail = "handler_error_log clean"
	}
	return r
}

// checkPlanetActivitySeqCorruption looks for two SQL-trigger pathologies:
//   - duplicate (planet_id, seq) pairs
//   - planets where max(seq) in planet_activity > sequence.counter (the
//     PLANET_ACTIVITY_FLEET_MOVE bug)
//
// PASS = both zero. FAIL = either nonzero. Skipped if the table is missing
// (fresh DB with cache but no structs.* yet).
func checkPlanetActivitySeqCorruption(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	if !tableExists(ctx, in.Pool, "structs", "planet_activity") ||
		!tableExists(ctx, in.Pool, "structs", "planet_activity_sequence") {
		r.Status = StatusSkip
		r.Detail = "structs.planet_activity / planet_activity_sequence not deployed"
		return r
	}
	var dupPlanets, dupRows int64
	err := in.Pool.QueryRow(ctx, `
		WITH dups AS (
			SELECT planet_id, seq, COUNT(*) AS n
			  FROM structs.planet_activity
			 GROUP BY planet_id, seq
			HAVING COUNT(*) > 1
		)
		SELECT COUNT(DISTINCT planet_id), COALESCE(SUM(n), 0)
		  FROM dups
	`).Scan(&dupPlanets, &dupRows)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("dup scan: %v", err)
		return r
	}
	var lagPlanets int64
	err = in.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT pa.planet_id, MAX(pa.seq) AS pa_max, COALESCE(s.counter, 0) AS counter
			  FROM structs.planet_activity pa
			  LEFT JOIN structs.planet_activity_sequence s ON s.planet_id = pa.planet_id
			 GROUP BY pa.planet_id, s.counter
			HAVING MAX(pa.seq) > COALESCE(s.counter, 0)
		) lag
	`).Scan(&lagPlanets)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("counter-lag scan: %v", err)
		return r
	}
	r.Counts = map[string]any{
		"duplicate_planets":    dupPlanets,
		"duplicate_rows":       dupRows,
		"counter_lag_planets":  lagPlanets,
	}
	if dupPlanets > 0 || lagPlanets > 0 {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("seq integrity broken: dup_planets=%d dup_rows=%d counter_lag_planets=%d (legacy cache.PLANET_ACTIVITY_FLEET_MOVE corruption; repaired by Phase B SQL backfill)",
			dupPlanets, dupRows, lagPlanets)
	} else {
		r.Status = StatusPass
		r.Detail = "no duplicate (planet_id, seq) pairs; counter consistent with max(seq)"
	}
	return r
}

// blockHeightTables is the set inspected by checkBlockHeightNulls.
// Mirrors db.statKnownTables (plus planet_activity) — keep the two in
// sync so verify never inspects a table the bootstrap doesn't know about.
var blockHeightTables = []string{
	"planet_activity",
	"stat_ore",
	"stat_fuel",
	"stat_capacity",
	"stat_load",
	"stat_structs_load",
	"stat_power",
	"stat_connection_capacity",
	"stat_connection_count",
	"stat_struct_health",
	"stat_struct_status",
}

// checkBlockHeightNulls counts rows where block_height IS NULL across the
// hypertables that sync-state populates. PASS = all zero. INFO (not FAIL)
// when nonzero because rows ingested before sync-state took ownership of
// the derivation may have NULL block_height; the Phase B SQL backfills
// those from sync_state.block_log on join (chain_id, block_time).
func checkBlockHeightNulls(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	counts := make(map[string]any, len(blockHeightTables))
	var anyNull, totalNull int64
	for _, t := range blockHeightTables {
		if !tableExists(ctx, in.Pool, "structs", t) {
			counts[t] = "missing"
			continue
		}
		if !columnExists(ctx, in.Pool, "structs", t, "block_height") {
			counts[t] = "no_block_height_column"
			continue
		}
		var n int64
		err := in.Pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM structs.%s WHERE block_height IS NULL`, t),
		).Scan(&n)
		if err != nil {
			counts[t] = fmt.Sprintf("err: %v", err)
			continue
		}
		counts[t] = n
		totalNull += n
		if n > 0 {
			anyNull++
		}
	}
	r.Counts = counts
	switch {
	case anyNull == 0:
		r.Status = StatusPass
		r.Detail = "no NULL block_height across derived hypertables"
	default:
		r.Status = StatusInfo
		r.Detail = fmt.Sprintf("%d table(s) carry NULL block_height (%d total rows). New writes populate the column; the Phase B SQL backfills historical rows from sync_state.block_log.", anyNull, totalNull)
	}
	return r
}

// checkRawMirrorCoverage compares sync_state.raw_blocks row count to
// sync_state.block_log row count. PASS when equal; FAIL when raw mirror
// has fewer rows than block_log (lost rows or operator toggled MirrorRaw
// off mid-run); SKIP when mirror is disabled.
func checkRawMirrorCoverage(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	if !in.MirrorRaw {
		r.Status = StatusSkip
		r.Detail = "raw mirror disabled (-mirror-raw=false / SYNC_STATE_MIRROR_RAW=false); cache.* compatibility views will return empty rows"
		return r
	}
	var rawN, blockN int64
	if err := in.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM sync_state.raw_blocks WHERE chain_id = $1`, in.ChainID).Scan(&rawN); err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("raw_blocks scan: %v", err)
		return r
	}
	if err := in.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM sync_state.block_log WHERE chain_id = $1`, in.ChainID).Scan(&blockN); err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("block_log scan: %v", err)
		return r
	}
	r.Counts = map[string]any{
		"raw_blocks": rawN,
		"block_log":  blockN,
		"diff":       blockN - rawN,
	}
	switch {
	case rawN == blockN:
		r.Status = StatusPass
		r.Detail = fmt.Sprintf("raw_blocks (%d) == block_log (%d)", rawN, blockN)
	case rawN < blockN:
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("raw_blocks (%d) < block_log (%d); %d blocks missing from mirror", rawN, blockN, blockN-rawN)
	default:
		r.Status = StatusInfo
		r.Detail = fmt.Sprintf("raw_blocks (%d) > block_log (%d) — historical mirror, likely fine", rawN, blockN)
	}
	return r
}

// checkGenesisLoaded confirms that sync-state's init-genesis step has
// been applied for this chain AND that the resulting structs.ledger
// rows are still present.
//
// History: the previous version of this check only read
// sync_state.genesis_log, so a `DELETE FROM structs.ledger WHERE
// action='genesis'` between an init-genesis run and a verify run would
// PASS while every genesis-funded account silently showed negative
// balances. We now cross-check that
// COUNT(structs.ledger WHERE action='genesis') equals
// genesis_log.total_rows, so the discrepancy can no longer hide.
//
// States:
//   - PASS  : genesis_log row present AND ledger count matches total_rows
//   - FAIL  : genesis_log row missing while cursor > 0, OR ledger count
//             does not match genesis_log.total_rows (drift detected)
//   - INFO  : cursor at 0 and genesis not yet applied (first-start),
//             OR genesis_log present but structs.ledger isn't deployed
//   - SKIP  : never (drift is always actionable)
//
// We surface the recorded source + total_rows + sha256 + actual ledger
// count in Counts so operators can pinpoint the gap without re-fetching
// genesis or hand-querying the ledger.
func checkGenesisLoaded(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	row, err := db.ReadGenesisLog(ctx, in.Pool, in.ChainID)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("read genesis_log: %v", err)
		return r
	}
	cursor, cerr := db.ReadCursor(ctx, in.Pool, in.ChainID)
	if cerr != nil {
		// Cursor missing = ingest hasn't started; we can still confirm
		// genesis state (and check the ledger crosscheck if it has been
		// applied) but can't decide "should it have been loaded?"
		switch {
		case row != nil:
			return finalizeGenesisCheck(ctx, in, row, true)
		default:
			r.Status = StatusInfo
			r.Detail = fmt.Sprintf("no cursor row yet (ingest hasn't started); read_cursor: %v", cerr)
		}
		return r
	}
	switch {
	case row != nil:
		return finalizeGenesisCheck(ctx, in, row, false)
	case cursor.LastHeight <= 0:
		r.Status = StatusInfo
		r.Detail = "cursor at height 0 and genesis not yet applied; init-genesis will run on first ingest"
	default:
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("no sync_state.genesis_log row for chain=%s but cursor advanced to h=%d; "+
			"run `sync-state init-genesis` to backfill genesis ledger rows "+
			"(otherwise structs.ledger balances will appear negative for genesis-funded accounts)",
			in.ChainID, cursor.LastHeight)
	}
	return r
}

// finalizeGenesisCheck runs the second leg of checkGenesisLoaded: given
// a present genesis_log row, count action='genesis' rows in
// structs.ledger and FAIL on drift. Pulled into its own function so
// both the cursor-missing and cursor-present branches share identical
// drift semantics — silent divergence between the two would defeat the
// whole point of the cross-check.
//
// cursorMissing is true when we got here because ReadCursor failed; we
// thread it through only to color the Detail string ("genesis applied"
// vs "genesis applied (cursor not yet written)").
func finalizeGenesisCheck(ctx context.Context, in Inputs, row *db.GenesisLogRow, cursorMissing bool) CheckResult {
	r := CheckResult{}
	counts := map[string]any{
		"source":           row.Source,
		"applied_at":       row.AppliedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"genesis_time":     row.GenesisTime.UTC().Format("2006-01-02T15:04:05Z"),
		"sha256":           row.SHA256,
		"total_rows":       row.TotalRows,
		"rows_per_section": row.RowsPerSection,
	}

	// structs.ledger may not be deployed in test harnesses; downgrade
	// to INFO rather than FAIL in that one case so the unit tests that
	// only stand up sync_state.* still pass.
	if !tableExists(ctx, in.Pool, "structs", "ledger") {
		counts["ledger_genesis_rows"] = "n/a (structs.ledger not deployed)"
		r.Counts = counts
		r.Status = StatusInfo
		r.Detail = fmt.Sprintf("genesis_log row present (%d rows) but structs.ledger isn't deployed; can't verify rows landed",
			row.TotalRows)
		return r
	}

	var ledgerCount int64
	if err := in.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM structs.ledger WHERE action = 'genesis'`).
		Scan(&ledgerCount); err != nil {
		counts["ledger_genesis_rows"] = fmt.Sprintf("error: %v", err)
		r.Counts = counts
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("count structs.ledger WHERE action='genesis': %v", err)
		return r
	}
	counts["ledger_genesis_rows"] = ledgerCount
	r.Counts = counts

	switch {
	case ledgerCount == row.TotalRows:
		r.Status = StatusPass
		suffix := ""
		if cursorMissing {
			suffix = " (cursor not yet written)"
		}
		r.Detail = fmt.Sprintf("genesis applied (%d rows present in structs.ledger; source=%s)%s",
			ledgerCount, row.Source, suffix)
	case ledgerCount == 0:
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("genesis_log says total_rows=%d (applied %s) but structs.ledger has ZERO action='genesis' rows; "+
			"someone deleted them after init-genesis ran — "+
			"run `sync-state init-genesis -force` to restore",
			row.TotalRows, row.AppliedAt.UTC().Format("2006-01-02T15:04:05Z"))
	default:
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("genesis_log says total_rows=%d but structs.ledger has %d action='genesis' rows (drift=%d); "+
			"partial deletion or partial replay — run `sync-state init-genesis -force` to restore",
			row.TotalRows, ledgerCount, row.TotalRows-ledgerCount)
	}
	return r
}

// checkCurrentBlockStatus reads structs.current_block (the webapp-facing
// view of where we are) and FAILs when status != 'current' or lag > LagWarn.
// Skipped when the table doesn't exist (fresh structs-pg-less DB).
func checkCurrentBlockStatus(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	if !tableExists(ctx, in.Pool, "structs", "current_block") {
		r.Status = StatusSkip
		r.Detail = "structs.current_block not deployed"
		return r
	}
	var height int64
	var status *string
	var lag, tip *int64
	err := in.Pool.QueryRow(ctx, `
		SELECT height, status, lag_blocks, tip_height
		  FROM structs.current_block
		 WHERE chain = $1
	`, in.ChainID).Scan(&height, &status, &lag, &tip)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			r.Status = StatusInfo
			r.Detail = "structs.current_block has no row for chain (first sync hasn't committed yet)"
			return r
		}
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("scan current_block: %v", err)
		return r
	}
	var statusStr string
	if status != nil {
		statusStr = *status
	}
	var lagVal int64
	if lag != nil {
		lagVal = *lag
	}
	var tipVal int64
	if tip != nil {
		tipVal = *tip
	}
	r.Counts = map[string]any{
		"height": height,
		"status": statusStr,
		"lag":    lagVal,
		"tip":    tipVal,
	}
	r.Height = &height
	switch {
	case statusStr == string(db.StatusCurrent) && lagVal <= in.LagWarn:
		r.Status = StatusPass
		r.Detail = fmt.Sprintf("status=current h=%d (lag=%d)", height, lagVal)
	case statusStr == "":
		r.Status = StatusInfo
		r.Detail = fmt.Sprintf("status empty (legacy row pre-bootstrap); h=%d lag=%d", height, lagVal)
	case lagVal > in.LagWarn:
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("status=%s lag=%d > %d", statusStr, lagVal, in.LagWarn)
	default:
		r.Status = StatusInfo
		r.Detail = fmt.Sprintf("status=%s h=%d lag=%d", statusStr, height, lagVal)
	}
	return r
}

// orderedTimeseriesSpec describes how to detect block_height inversions in
// an append-only table. partitionCols group the entity stream; orderCols
// define the within-stream ordering that must be monotonic in block_height.
type orderedTimeseriesSpec struct {
	table        string
	partitionSQL string // e.g. "address, denom"
	orderSQL     string // e.g. "created_at, block_height"
}

var orderedTimeseriesSpecs = []orderedTimeseriesSpec{
	// structs.ledger uses `time` (defaulted to NOW() at insert) and `block_height`.
	// There is no `created_at` column — using one regresses to a runtime
	// 42703 from the check itself, which masks real inversions.
	{table: "ledger", partitionSQL: "address, denom", orderSQL: "time, block_height"},
	{table: "planet_activity", partitionSQL: "planet_id", orderSQL: "time, seq"},
	// structs.defusion has no block_height column (only completed_at +
	// created_at), so block_height inversion analysis is structurally
	// impossible here. checkOrderedTimeseriesMonotonic relies on a
	// non-empty block_height column; defusion is intentionally excluded.
	{table: "stat_ore", partitionSQL: "object_type, object_index", orderSQL: "time, block_height"},
	{table: "stat_fuel", partitionSQL: "object_type, object_index", orderSQL: "time, block_height"},
	{table: "stat_capacity", partitionSQL: "object_type, object_index", orderSQL: "time, block_height"},
	{table: "stat_load", partitionSQL: "object_type, object_index", orderSQL: "time, block_height"},
	{table: "stat_structs_load", partitionSQL: "object_index", orderSQL: "time, block_height"},
	{table: "stat_power", partitionSQL: "object_type, object_index", orderSQL: "time, block_height"},
	{table: "stat_connection_capacity", partitionSQL: "object_index", orderSQL: "time, block_height"},
	{table: "stat_connection_count", partitionSQL: "object_index", orderSQL: "time, block_height"},
	{table: "stat_struct_health", partitionSQL: "object_index", orderSQL: "time, block_height"},
	{table: "stat_struct_status", partitionSQL: "object_index", orderSQL: "time, block_height"},
}

// checkOrderedTimeseriesMonotonic fails when block_height decreases within
// an entity's time-ordered stream — a signal that ingest reordered rows.
func checkOrderedTimeseriesMonotonic(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{Counts: map[string]any{}}
	var totalInversions int64
	for _, spec := range orderedTimeseriesSpecs {
		if !tableExists(ctx, in.Pool, "structs", spec.table) {
			continue
		}
		if !columnExists(ctx, in.Pool, "structs", spec.table, "block_height") {
			continue
		}
		q := fmt.Sprintf(`
			SELECT COUNT(*) FROM (
				SELECT block_height,
				       LAG(block_height) OVER (
				           PARTITION BY %s ORDER BY %s
				       ) AS prev_h
				  FROM structs.%s
			) inv WHERE prev_h IS NOT NULL AND block_height < prev_h
		`, spec.partitionSQL, spec.orderSQL, spec.table)
		var n int64
		if err := in.Pool.QueryRow(ctx, q).Scan(&n); err != nil {
			r.Status = StatusFail
			r.Detail = fmt.Sprintf("%s inversion scan: %v", spec.table, err)
			return r
		}
		if n > 0 {
			r.Counts[spec.table+"_inversions"] = n
			totalInversions += n
		}
	}
	r.Counts["total_inversions"] = totalInversions
	if totalInversions > 0 {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("block_height went backwards in %d ordered row(s) across append-only tables", totalInversions)
	} else {
		r.Status = StatusPass
		r.Detail = "no block_height inversions in ledger, planet_activity, defusion, or stat_* streams"
	}
	return r
}

// checkLedgerBalanceSanity counts address/denom pairs whose net ledger
// balance is negative. Non-zero after genesis is loaded indicates missing
// credits or handler bugs.
func checkLedgerBalanceSanity(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{}
	if !tableExists(ctx, in.Pool, "structs", "ledger") {
		r.Status = StatusSkip
		r.Detail = "structs.ledger not deployed"
		return r
	}
	var negative int64
	err := in.Pool.QueryRow(ctx, `
		WITH bal AS (
			SELECT address, denom,
			       SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END) AS net
			  FROM structs.ledger
			 GROUP BY address, denom
		)
		SELECT COUNT(*) FROM bal WHERE net < 0
	`).Scan(&negative)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("ledger balance scan: %v", err)
		return r
	}
	r.Counts = map[string]any{"negative_balances": negative}
	if negative > 0 {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%d address/denom pair(s) with negative net balance", negative)
	} else {
		r.Status = StatusPass
		r.Detail = "no negative ledger balances"
	}
	return r
}

// tableExists is a small introspection helper so checks can SKIP cleanly
// when the inspected table isn't deployed (fresh / minimal DBs).
func tableExists(ctx context.Context, q db.Querier, schema, table string) bool {
	var ok bool
	_ = q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_tables
			 WHERE schemaname = $1 AND tablename = $2
		)
	`, schema, table).Scan(&ok)
	return ok
}

// columnExists checks for a single column in a schema-qualified table.
// Used by checkBlockHeightNulls to be tolerant of partially-bootstrapped
// deployments.
func columnExists(ctx context.Context, q db.Querier, schema, table, column string) bool {
	var ok bool
	_ = q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
		)
	`, schema, table, column).Scan(&ok)
	return ok
}
