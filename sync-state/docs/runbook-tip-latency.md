# Tip-latency runbook

How fast does sync-state see a new block after the chain commits it?
This runbook explains the two paths (WebSocket push, poll failsafe),
the operator-visible knobs, and how to tune for either lowest latency
or maximum compatibility.

## Why this matters

A user submits a tx -> the chain commits it in a block -> sync-state
ingests the block -> the webapp queries the resulting row. Every layer
adds latency:

| Stage                                | Typical | Worst-case |
|--------------------------------------|---------|------------|
| Tx hits mempool -> block commit      | 3 s     | 6 s        |
| sync-state notices new block         | <1 s    | -poll (1s) |
| sync-state fetches block + applies   | ~200 ms | ~500 ms    |
| Webapp poll picks up the new row     | webapp's loop, out of our scope |

Before this change the "sync-state notices" stage was bounded by the
poll interval (then 3 s), so the worst-case player-action-to-result
latency was ~10 s. With the WebSocket push enabled it drops to
"network RTT after commit" — ~200-500 ms in normal conditions.

## How it works

sync-state opens a single CometBFT WebSocket subscription to
`tm.event='NewBlock'`. Every committed block triggers a push frame
containing the new height; sync-state's tip-idle loop selects on that
push channel and immediately re-polls `/status` to recompute the
target window.

The poll interval (`-poll`, default `1s`) remains as a **failsafe**.
It covers three cases the WebSocket alone cannot:

1. The dial loop is still reconnecting after a disconnect.
2. The remote node accepted the upgrade but stopped emitting events
   (e.g. its own node is catching up).
3. The operator turned the WebSocket off (`-tip-ws=false`).

In all cases the worst-case tip-detection latency is still bounded
by `-poll`.

### Endpoint selection

The notifier walks the same ordered pool as the JSON-RPC client
(primary first, seed last). If the primary either fails the
WebSocket upgrade or stays silent for 60s, the notifier rotates to
the next endpoint. The JSON-RPC pool and the WebSocket pool advance
independently — the primary can serve `/block` fine while a fallback
serves push events, and vice versa.

## Configuration

| Flag / env var                       | Default | Notes                                                         |
|--------------------------------------|---------|---------------------------------------------------------------|
| `-tip-ws` / `SYNC_STATE_TIP_WS`      | `true`  | Enable the NewBlock WebSocket subscription                    |
| `-poll`   / `SYNC_POLL_INTERVAL`     | `1s`    | Failsafe sleep between tip-polls when caught up               |

Recommended settings:

- **Production with structsd alongside**: leave defaults. WebSocket
  works against a local structsd; poll failsafe rarely fires.
- **Cloud / public RPC**: leave defaults. The public testnet node
  exposes `/websocket` correctly; we verified this with
  `cmd/wsprobe`.
- **Restricted network (no Upgrade)**: `-tip-ws=false -poll=1s`. The
  pure-poll path has the same worst-case latency as the failsafe.
- **Low-priority backfill bot**: `-tip-ws=false -poll=5s`. There's no
  point pinging the node every second when no one is waiting on the
  data.

## Verifying

### At startup

The doctor probes the WebSocket reachability of every configured
endpoint. Look for `tip websocket OK` in the per-endpoint section:

```
[primary] http://structsd:26657 (role=primary)
  OK    chain_id              structstestnet-111
  OK    node liveness         tip=803975 catching_up=false
  OK    earliest_block_height earliest=1 (archive)
  OK    tip websocket         ws://structsd:26657/websocket reachable; ...
```

A `WARN tip websocket upgrade ... failed` means push won't work for
that endpoint and the failsafe poll will be used. Sync still runs;
latency just degrades to the `-poll` setting.

### At runtime

The notifier logs `tip-ws: notifier connected to <url> (subscribed
NewBlock)` once it has a live subscription. Reconnects log
`tip-ws: notifier disconnected from <url>: <err> (reconnecting in
<backoff>)`. Endpoint rotations log
`tip-ws: notifier rotating endpoint A -> B (reason: <err>)`.

### Manual smoke test

The repo ships a tiny `wsprobe` binary that just listens for NewBlock
pushes against an arbitrary RPC URL for 25s:

```
go run ./cmd/wsprobe -rpc https://public.testnet.structs.network:26657
```

If you see one `got NewBlock h=...` line per block (~6s apart), the
endpoint is good. If you see only `notifier connected` but no events,
the node accepts the upgrade but isn't pushing — usually because its
own underlying node is catching up.

## Latency budget after this change

With both knobs at default and a healthy primary:

| Stage                                | Latency  |
|--------------------------------------|----------|
| Block commit -> WebSocket push       | ~50 ms   |
| Push -> sync-state /status re-poll   | ~10 ms   |
| /status -> /block + /block_results   | ~150 ms  |
| Apply block to DB (single tx)        | ~30 ms   |
| **Total: commit -> committed row**   | **~250 ms** |

With `-tip-ws=false` (pure poll):

| Stage                                | Latency  |
|--------------------------------------|----------|
| Block commit -> next poll tick       | avg 500 ms, max 1 s |
| /status -> /block + /block_results   | ~150 ms  |
| Apply block to DB                    | ~30 ms   |
| **Total: commit -> committed row**   | **~700 ms avg, 1.2 s max** |

In both cases the actual lag is dominated by the WebSocket/poll
detection — the apply step is fast.

## Slow-block phase timings

`-slow-block-threshold` / `SYNC_SLOW_BLOCK_THRESHOLD` defaults to `2s`.
Set it to `0s` to disable timing collection. Successful and failed applies
emit one JSON object (after the standard log timestamp) when they cross the
threshold. The existing window progress logs remain available for every window.

- `slow_block`: threshold covers fetch/decode plus apply. `queue_wait_ms`
  separately measures time a fetched block waited for its turn; it does not
  contribute to the threshold. Fetch includes RPC retries and validation.
- `slow_window`: threshold covers the bulk transaction's apply duration.
  `fetch_sum_ms` is the sum of overlapping fetches, **not** window wall time.
- `apply_ms` includes opening the transaction, database work, commit/rollback,
  and writing handler-error diagnostics. `outcome` is `committed`, `failed`,
  or `commit_unconfirmed` (a commit error cannot prove whether PostgreSQL committed).
- `phases_ms` separates `projection_lock`, `dispatch`, `bank`,
  `native_invalidation`, `buffer_flush`, `coverage`, `projections`, `raw_mirror`,
  `cursor_heartbeat_audit`, and `commit`, plus setup/error/reconciliation phases
  when used. Bulk windows aggregate per-block phases and add `bulk_stats_flush`
  and `bulk_raw_flush`.
- `projection.*` entries are subphases **within** `projections`; do not add them
  to the parent. They distinguish expansion, inventory, each leaderboard,
  watermark updates, and `projection.work`. Counts include affected projection
  IDs and work rows inserted, updated, and deleted.

Timings are application elapsed time, so a phase includes database waits and
client/server round trips. They do not replace query plans or PostgreSQL
statistics. Fetch failures that never produce a block bundle retain their
ordinary RPC error logs. Payloads, addresses, SQL and error text are excluded
from timing records. Deferred catch-up projection rebuilds retain their
separate existing rebuild logs.

### Work-refresh investigation (2026-11-08)

An isolated copy of public game state, with 1,943 conservative dirty keys
inferred from block 3,494,624, reproduced a warm work refresh of about 387 ms.
The candidate-row query dominated: three scans through
`struct_updated_at_id_idx` took about 114–117 ms each. Deletion and upsert
were about 6 ms combined. Adding statistics to the dirty temporary table
made no material improvement; joining the target list directly was slower.

An experimental per-target lookup (`unnest(v_keys)` with a correlated
`view.work_live` query and `OFFSET 0` to retain the lookup boundary) reduced
warm refresh time to about 52–57 ms. Empty, single-target, and burst tests
produced identical cached work contents; the single-target case did not
regress. Both forced custom and generic plans improved. This is implemented by
`function-api-work-20261108-target-lookups` in structs-pg, with an exact
revert to the previous function. The migration retains locking, dirty-key
expansion, diff semantics, watermark updates, and the full-refresh fallback.
The data represents current state, not a replay of the historical block's
pre-state; actual trigger queue membership may be smaller than this
conservative set. The existing full-refresh path above 2,000 dirty keys
was not changed by the experiment.
