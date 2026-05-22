# Bulk-load mode (deferred commit + buffered COPY)

Bulk mode reduces catch-up time in two layers:

1. **Phase 1 — deferred commit.** Every **N blocks** commit in one PostgreSQL
   transaction instead of one transaction per block.
2. **Phase 2 — buffered COPY.** Every append-only structs.* row written during
   the window (ledger, defusion, planet_activity, stat_*) is buffered in memory
   and flushed via `pgx.CopyFrom` immediately before the outer commit. Streaming
   mode uses the same buffer with a per-block flush — UPSERT/UPDATE writes
   continue to run inline so `IS DISTINCT FROM` guards and prev-row reads keep
   their normal semantics.

**Event order, handler logic, and per-row INSERT/UPSERT semantics are
unchanged** — only commit frequency, cursor/heartbeat cadence, and the wire
protocol used for the append-only inserts differ.

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

There is no separate log line for "buffer flushed N rows" — the flush is part
of the per-window commit. If you need to spot-check, run a verify that
includes `ordered_timeseries_monotonic` and `ledger_balance_sanity`
immediately after a window; both checks read the same tables the buffer
flushed.

## Data integrity

Bulk mode preserves chain order:

1. Blocks processed strictly ascending
2. Events within each block in RPC order (finalize → per-tx)
3. Per-handler SAVEPOINTs unchanged — a handler-level failure rolls back its
   savepoint AND truncates any rows it appended to the buffer
4. `sync_state.block_log` still one row per block inside the outer tx
5. On failure, the whole window rolls back; cursor unchanged; retry is safe

Phase 2 buffer flush ordering:

| Table | Order within flush | Order across rows |
|-------|--------------------|-------------------|
| `structs.ledger` | append order | block order, then chain event order |
| `structs.defusion` | append order | block order, then chain event order |
| `structs.planet_activity` | append order; `seq` assigned by `nextPlanetActivitySeq` UPSERT at handler time, so `(planet_id, seq)` stays unique and monotonic | block order, then chain event order |
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
- `sync_state.raw_*` mirror (when `-mirror-raw` is set) still uses
  `pgx.CopyFrom` per block — not window-batched. That extra batching is
  bookkeeping-only and stays a Phase 3 nice-to-have.
