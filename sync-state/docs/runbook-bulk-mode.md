# Bulk-load mode (deferred commit + buffered COPY)

Bulk mode commits a window of blocks in one PostgreSQL transaction. The writer
applies blocks in chain order and flushes authoritative ledger, activity and
stat rows per block so subsequent handlers can read the current state. It
advances the cursor and emits a heartbeat when the window commits.

With `-mirror-raw`, bulk mode groups raw event and attribute copies within the
window transaction. It flushes after a complete block when the buffer reaches
32,768 combined event/attribute rows and flushes the remainder before updating
the cursor. Raw block and transaction-result records still use the per-block path.

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
| `-bulk-window` | `SYNC_STATE_BULK_WINDOW` | `500` | Maximum blocks per bulk transaction |
| `-bulk-lag-threshold` | `SYNC_STATE_BULK_LAG_THRESHOLD` | `50` | Min lag to enter bulk |
| `-bulk-statement-timeout` | `SYNC_STATE_BULK_STATEMENT_TIMEOUT` | `5m` | Outer tx statement timeout |
| `-bulk-async-commit` | `SYNC_STATE_BULK_ASYNC_COMMIT` | `true` | `synchronous_commit=off` on bulk windows |
| `-bulk-defer-projections` | `SYNC_STATE_BULK_DEFER_PROJECTIONS` | `true` | Skip `api_*` recompute during catch-up; rebuild all once before streaming resumes (`false` = once per window) |

During bulk catch-up, sync-state fetches the next `-bulk-window` blocks while
it applies the current window. `-parallelism` controls the number of concurrent
block fetches. In streaming mode, `-batch` sets the fetch window size.

## Observability

Log lines include mode:

```
fetched h=5000 (window 4501-5000, bulk, 5865.9 blocks/s, lag=81603)
window done (bulk): 500 blocks [4501..5000] in 682ms (733.7 blocks/s)
```

At tip:

```
window done (stream): 1 blocks [809638..809638] in 38ms (26.6 blocks/s)
```

There is no separate log line for "buffer flushed N rows" — the flush is part
of the per-window commit. If you need to spot-check, run a verify that
includes `ordered_timeseries_monotonic` and `ledger_balance_sanity`
immediately after a window; both checks read the same tables the buffer
flushed.

## Data integrity

Bulk mode preserves chain order:

1. Blocks processed strictly ascending
2. Events within each block in protocol execution order (`rpc.OrderedEvents`)
3. Handlers run directly in the block/window transaction. A required error or
   panic stops ingestion, rolls back the whole transaction, and discards its
   buffered rows and dirty sets. Explicit harmless warnings may continue.
4. `sync_state.block_log` still one row per block inside the outer tx
5. On failure, the whole window rolls back; cursor unchanged; retry is safe

Error reprocessing retains per-handler SAVEPOINTs so it can isolate a failed
replay and continue. Attribute upserts use PostgreSQL 18 `RETURNING OLD` to
capture the previous value without a separate SELECT. Identical values remain
no-ops; activity details preserve the exact old and new uint64 values.

Buffer flush ordering:

| Table | Order within flush | Order across rows |
|-------|--------------------|-------------------|
| `structs.ledger` | append order | block order, then chain event order |
| `structs.defusion` | append order | block order, then chain event order |
| `structs.planet_activity` | append order; sequence ranges allocated once per nonempty block and planet at flush time, preserving unique, monotonic `(planet_id, seq)` | block order, then chain event order |
| `structs.stat_*` | append order | block order, then chain event order |

`time` on `structs.stat_*` rows now comes from `bctx.BlockTime` instead of
`NOW()`. The legacy `NOW()` value drifted further from `block_time` the longer
a backfill took; the new value matches the block being processed and lands in
the correct TimescaleDB chunk.

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

## What bulk mode does *not* change

- No parallel block apply
- No handler SQL changes for UPSERT/UPDATE paths (struct, fleet, planet,
  infusion, grid, planet_raid, etc. still flow through `tx.Exec` with the
  `IS DISTINCT FROM` guards intact)
- Webapp at tip: unchanged (streaming mode emits `pg_notify('grass', …)` per
  block as before)
