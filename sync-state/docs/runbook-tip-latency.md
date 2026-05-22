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
