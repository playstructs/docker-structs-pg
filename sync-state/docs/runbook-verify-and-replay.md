# Operator runbook: `sync-state verify` and `sync-state reprocess-errors`

> Related runbooks:
> [`runbook-ownership-flags.md`](runbook-ownership-flags.md) (what sync-state owns),
> [`runbook-rpc-failover.md`](runbook-rpc-failover.md) (primary/seed RPC pool).

These two subcommands together cover the day-2 operator loop:
**look at the system** (`verify`) and **fix what's broken** (`reprocess-errors`).
Both are idempotent and safe to run alongside an active ingest.

---

## `sync-state verify`

Runs a fixed set of data-quality checks against `sync_state.*`,
`structs.*`, and the live RPC tip, persists the results to
`sync_state.verification_report`, and prints a human-readable summary.

### Quick recipes

```shell
# Default: run every check, write to verification_report, print text.
sync-state verify

# Only check for unresolved handler errors (cheap; suitable for cron).
sync-state verify -errors-only

# Machine-readable for hooks / monitoring.
sync-state verify -json
```

### Exit codes

| Code | Meaning |
|------|---------|
| 0    | All checks passed or returned INFO/SKIP. |
| 1    | At least one check returned FAIL (or an infrastructure error). |
| 2    | Invalid args. |

### Check catalogue

| Check | What it tests | Notes |
|-------|---------------|-------|
| `cursor_vs_tip` | `sync_state.sync_cursor.last_height` vs live RPC tip. | INFO when the upstream node is catching_up (the lag is real but expected). |
| `block_log_coverage` | `sync_state.block_log` is contiguous from min to max height. | A gap means a fetch failure was swallowed somewhere. |
| `handler_errors_unresolved` | Unresolved rows in `sync_state.handler_error_log`. | FAIL when any `severity='error'`; INFO when only warns remain. |
| `planet_activity_seq_corruption` | Duplicate `(planet_id, seq)` pairs + `planet_activity.seq > planet_activity_sequence.counter`. | Symptom of the historical `PLANET_ACTIVITY_FLEET_MOVE` SQL bug. New writes by sync-state are correct; the Phase B SQL repairs the historical rows. |
| `block_height_nulls` | `block_height IS NULL` count across `planet_activity` and `stat_*`. | INFO (not FAIL) when nonzero — new writes populate the column; the Phase B SQL backfills historical rows from `sync_state.block_log`. |
| `raw_mirror_coverage` | `sync_state.raw_blocks` row count == `sync_state.block_log` row count. | Default-on (powers the `cache.*` compatibility views). Skipped when explicitly disabled with `-mirror-raw=false`. |
| `current_block_status` | `structs.current_block.status='current'` and `lag_blocks <= lag-warn`. | The webapp's heartbeat row; `-lag-warn` defaults to 5 blocks. |
| `genesis_loaded` | `sync_state.genesis_log` has a row for this chain. | FAIL when the cursor advanced past height 0 but no row is present (causes the false-alarm "negative balance" rows — staking debits with no matching genesis credits). Fix: `sync-state init-genesis`. See [`runbook-init-genesis.md`](runbook-init-genesis.md). |
| `ordered_timeseries_monotonic` | `block_height` never decreases within ordered streams in `ledger`, `planet_activity`, `defusion`, and `stat_*`. | FAIL on any inversion — signals ingest reordered append-only rows. Useful after bulk catch-up. |
| `ledger_balance_sanity` | No address/denom pairs with negative net balance in `structs.ledger`. | FAIL when any negative balance exists post-genesis. |

### Knobs

| Flag | Default | Notes |
|------|---------|-------|
| `-errors-only` | `false` | Only run `handler_errors_unresolved`. |
| `-write-report` | `true` | Persist results to `sync_state.verification_report`. Set false for read-only probes. |
| `-json` | `false` | Emit JSON instead of text. |
| `-lag-warn` | `5` | Lag threshold for FAILing `current_block_status`. |

### Reading the report

`sync_state.verification_report` is intentionally append-only — every run
gets its own `run_id`. To see only the latest run:

```sql
SELECT scope, status, actual, created_at
  FROM sync_state.verification_report
 WHERE run_id = (
   SELECT run_id FROM sync_state.verification_report
    ORDER BY created_at DESC LIMIT 1
 )
 ORDER BY scope;
```

---

## `sync-state reprocess-errors`

Replays unresolved `sync_state.handler_error_log` rows against the
**current** handler code. Each row's block is refetched from the RPC
pool, the specific event is dispatched through the registered router,
and on success the row is marked `resolved_at=NOW()` with
`resolved_by='reprocess-errors'`.

### When to use

- Fresh after deploying a handler bug fix (resolve the rows it would
  now handle correctly).
- To clear the genesis `EventGrid` warns (see "Recipe: clear the
  genesis grid warns" below).

### Quick recipes

```shell
# Replay every unresolved error.
sync-state reprocess-errors

# Replay only one event type (e.g. after fixing parseGridAttributeID).
sync-state reprocess-errors -composite-key=structs.structs.EventGrid.gridRecord

# Replay warns instead of errors.
sync-state reprocess-errors -severity=warn

# Replay both severities.
sync-state reprocess-errors -severity=

# Replay a height range only.
sync-state reprocess-errors -since=1 -until=1000

# Preview (no commits, no resolve).
sync-state reprocess-errors -dry-run

# Safety cap (default 100).
sync-state reprocess-errors -limit=10
```

### Exit codes

| Code | Meaning |
|------|---------|
| 0    | Every selected row was resolved, skipped with a known reason, or — in dry-run — would-resolve cleanly. |
| 1    | At least one row produced a hard error during replay; check `stderr` for the message and the per-row table on `stdout`. |

### Output format

```text
id   height  composite_key                                       status
42   1       structs.structs.EventGrid.gridRecord                RESOLVED
43   1       structs.structs.EventGrid.gridRecord                SKIPPED  composite_key not found in fetched block (block may have been re-executed)
44   17      structs.structs.EventPlanet.planet                  FAILED   planet upsert id=2-193: ERROR: ... still broken

Summary: 1 resolved, 1 skipped, 1 failed
```

### Knobs

| Flag | Default | Notes |
|------|---------|-------|
| `-composite-key` | `""` | Match exact event type (e.g. `structs.structs.EventGrid.gridRecord`). Empty = any. |
| `-severity` | `error` | `error`, `warn`, or empty (both). |
| `-since`, `-until` | `0` | Height bounds. `0` means "no limit on that side". |
| `-limit` | `100` | Safety cap on rows processed per run. `0` = no cap. |
| `-dry-run` | `false` | Per-block tx is rolled back; no rows are marked resolved. |

### Safety properties

- Each replayed block runs in its own per-block transaction. A handler
  that fails leaves its row in `handler_error_log` AND rolls back its
  own writes (via the same SAVEPOINT isolation the ingester uses).
- Successful rows are marked resolved **after** the per-block commit,
  so we never end up with a "resolved" row whose writes were rolled
  back.
- The writer advisory lock is acquired, so an ingester running against
  the same chain will block until reprocess-errors exits. Run during a
  quiet window if you have a slow chain to walk.
- `-dry-run` is safe to run any time — it commits nothing and never
  touches `resolved_at`.

---

## Recipe: clear the genesis grid warns

The block-1 `EventGrid` with `attributeId="2-"` was previously rejected
by the Go handler (severity='error') and now lands as severity='warn'
post-fix. To clear stale rows from the old strict path:

```shell
# Inspect what's there first.
sync-state verify -errors-only

# Replay (resolve) every unresolved EventGrid row.
sync-state reprocess-errors \
  -composite-key=structs.structs.EventGrid.gridRecord \
  -severity=
```

If you'd rather just delete them (they're historical noise, not
actionable):

```sql
DELETE FROM sync_state.handler_error_log
 WHERE composite_key = 'structs.structs.EventGrid.gridRecord'
   AND resolved_at IS NULL;
```

The deletion path is appropriate for legacy strict-fail rows that the
current handler would still warn-skip on; the reprocess path is
appropriate when you want a clean audit trail (`resolved_by` shows
where the resolution came from).

---

## Recipe: post-deploy verification loop

After every `sync-state` release:

```shell
# 1. Confirm the new handler set still validates cleanly.
sync-state verify

# 2. If `handler_errors_unresolved` is FAIL but the bug-fix in this
#    release would resolve them, replay:
sync-state reprocess-errors -dry-run             # preview
sync-state reprocess-errors                       # commit

# 3. Verify the queue is now drained.
sync-state verify -errors-only
```

---

## Startup banner

On every `sync-state` (ingest) startup you'll see one of:

```text
Handler error log: 0 unresolved rows
```

or

```text
WARN: 3 unresolved handler_error_log rows (1 error, 2 warn).
  Inspect with: sync-state verify -errors-only
  Replay with:  sync-state reprocess-errors -since=<height>
```

This is sourced from the same `UnresolvedErrorSummary` helper `verify`
uses, so the numbers always agree.
