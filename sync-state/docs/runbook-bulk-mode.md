# Bulk-load mode (deferred commit)

Phase 1 bulk mode reduces catch-up time by committing every **N blocks** in one
PostgreSQL transaction instead of one transaction per block. **Event order,
handler logic, and per-row INSERT/UPSERT semantics are unchanged** — only commit
frequency and cursor/heartbeat cadence differ.

## When it activates

Bulk mode is **automatic** during catch-up:

- `-bulk-enabled=true` (default)
- `-bulk-lag-threshold=50` (default) — bulk when `tip - cursor >= 50`
- At tip (lag below threshold), sync-state returns to **streaming mode** (one tx
  per block, normal `pg_notify` cadence)

## Knobs

| Flag | Env | Default | Purpose |
|------|-----|---------|---------|
| `-bulk-enabled` | `SYNC_STATE_BULK_ENABLED` | `true` | Master switch |
| `-bulk-window` | `SYNC_STATE_BULK_WINDOW` | `100` | Blocks per outer tx |
| `-bulk-lag-threshold` | `SYNC_STATE_BULK_LAG_THRESHOLD` | `50` | Min lag to enter bulk |
| `-bulk-statement-timeout` | `SYNC_STATE_BULK_STATEMENT_TIMEOUT` | `5m` | Outer tx statement timeout |

`-batch` still controls RPC fetch parallelism. When bulk is active, the apply
window is capped at `-bulk-window`.

## Observability

Log lines include mode:

```
synced h=5000 (window 4901-5000, bulk, 180.2 blocks/s, lag=803609)
window done (bulk): 100 blocks [4901..5000] in 554ms (180.5 blocks/s)
```

At tip:

```
window done (stream): 1 blocks [809638..809638] in 38ms (26.6 blocks/s)
```

## Data integrity

Bulk mode preserves chain order:

1. Blocks processed strictly ascending
2. Events within each block in RPC order (finalize → per-tx)
3. Per-handler SAVEPOINTs unchanged
4. `sync_state.block_log` still one row per block inside the outer tx
5. On failure, the whole window rolls back; cursor unchanged; retry is safe

Verify after catch-up:

```bash
sync-state verify
```

New checks:

| Check | What it catches |
|-------|-----------------|
| `ordered_timeseries_monotonic` | `block_height` going backwards in ledger, planet_activity, stat_* |
| `ledger_balance_sanity` | Negative net balances per address/denom |

Manual ordered-table spot checks:

```sql
-- Ledger rows per block (no gaps in processed range)
SELECT block_height, COUNT(*) FROM structs.ledger
 WHERE block_height BETWEEN $min AND $max
 GROUP BY 1 ORDER BY 1;

-- Planet activity seq integrity
SELECT planet_id, MAX(seq) AS max_seq, s.counter
  FROM structs.planet_activity pa
  LEFT JOIN structs.planet_activity_sequence s USING (planet_id)
 GROUP BY planet_id, s.counter
 HAVING MAX(seq) > COALESCE(s.counter, 0);
```

## Disable bulk mode

```bash
sync-state ingest -bulk-enabled=false ...
```

Use this to compare streaming vs bulk catch-up or when debugging a suspected
ordering issue.

## Kill switch / failure behaviour

- Failed bulk window → full rollback, cursor stays at last committed height
- Re-run reprocesses the same blocks deterministically
- `-bulk-enabled=false` restores legacy one-tx-per-block behaviour instantly

## What bulk mode does *not* change (Phase 1)

- No COPY/batch INSERT yet (Phase 2 roadmap)
- No parallel block apply
- No handler SQL changes
- Webapp at tip: unchanged (streaming mode)
