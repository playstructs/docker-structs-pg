# Operator runbook: RPC primary + seed failover

`sync-state` reads blocks from a *pool* of CometBFT RPC endpoints. The
pool is at most two entries deep — a preferred **primary** (typically a
local `structsd`) and an always-on public **seed** — with primary tried
first and seed used as a transparent fallback when primary errors,
times out, or hasn't caught up to the requested block yet.

This document covers:

- the env-var contract,
- how to verify failover is working,
- what to expect when the primary is catching up,
- the deprecation path for the old single-URL knob.

## Env-var contract

| Flag                 | Env                    | Default                                            | Meaning                                                                                            |
|----------------------|------------------------|----------------------------------------------------|----------------------------------------------------------------------------------------------------|
| `-rpc-seed`          | `STRUCTS_RPC_SEED`     | `https://public.testnet.structs.network:26657`     | Always-on fallback. Must be reachable.                                                             |
| `-rpc-primary`       | `STRUCTS_RPC_PRIMARY`  | `""` (empty = use seed only)                       | Operator's preferred RPC. When set, becomes index-0 of the pool; seed slides to index-1.            |
| `-rpc`               | `STRUCTS_RPC_URL`      | `""`                                               | **Deprecated.** Still honored — treated as `-rpc-primary` and emits a one-line notice at startup. |

Resolution rules in [`internal/sync/config.go`](../internal/sync/config.go):

1. If `STRUCTS_RPC_PRIMARY` is set, it's the primary.
2. Otherwise, if the legacy `STRUCTS_RPC_URL` is set, that becomes the
   primary (and a `NOTICE:` is printed pointing operators at the new
   flags).
3. If neither is set, the pool is `[seed]` only.
4. `STRUCTS_RPC_SEED` always provides the fallback (default points at the
   public testnet RPC).

## Bootstrap-while-local-node-catches-up

Typical deployment scenario (e.g. `docker-structs-guild`):

1. Operator brings up `structsd` and `sync-state` in the same compose
   project. The compose unit sets:
   ```
   STRUCTS_RPC_PRIMARY=http://structsd:26657
   ```
   No `STRUCTS_RPC_SEED` is set, so the public default applies.
2. `structsd` starts state-sync, which takes minutes to hours.
3. During the catch-up window:
   - `sync-state` probes both endpoints. Primary is reachable and
     reports `catching_up=true` with a low tip; seed is fully caught up.
   - The doctor reports a per-endpoint `WARN node liveness` against
     primary (`catching_up=true at tip=N`); the pool verdict is
     `WARN: 2/2 endpoints healthy with primary still catching up`.
   - All block fetches transparently route through the seed because
     primary's cached `/status` has `tip < requested_height`.
4. As `structsd` catches up, its cached `/status` advances. Within one
   `statusTTL` window (5 s) after primary's tip crosses the requested
   block, the next block fetch hits primary directly.
5. Once primary is fully caught up, all subsequent fetches go to
   primary; the seed sits idle (no requests) until the next outage or
   reorg situation.

You can confirm the transition in the logs:

```
RPC pool (2 endpoint(s), primary first):
  [0] http://structsd:26657 (primary)
  [1] https://public.testnet.structs.network:26657 (fallback)
Connected: chain_id=structstestnet-111 tip=798200 earliest=1 catching_up=false
```

(The `Connected:` line reports the authoritative tip source: the first
caught-up endpoint in preference order. While primary is still
`catching_up=true`, this is the seed even though block fetches also
prefer seed for heights above primary's tip.)

## Verifying failover via the doctor

```
sync-state doctor
```

Sample output with a healthy pool:

```
Node compatibility check (preferred=http://structsd:26657):
  RPC pool:
    primary  http://structsd:26657
      chain_id                   OK    structstestnet-111
      node liveness              OK    tip=798200 catching_up=false
      earliest_block_height      OK    earliest=1 (archive)
    fallback https://public.testnet.structs.network:26657
      chain_id                   OK    structstestnet-111
      node liveness              OK    tip=798200 catching_up=false
      earliest_block_height      OK    earliest=1 (archive)
  rpc pool                   OK    2/2 endpoint(s) healthy, chain_id=structstestnet-111
  ...
```

Sample output with primary down:

```
  RPC pool:
    primary  http://structsd:26657
      reachable                  FATAL /status failed: dial tcp: connection refused
    fallback https://public.testnet.structs.network:26657
      chain_id                   OK    structstestnet-111
      ...
  rpc pool                   WARN  1 of 2 endpoints healthy; sync will proceed against reachable endpoints
  ...
  Verdict: ARCHIVE NODE, suitable for full backfill from height 1.
```

Note that **WARN is not FATAL** — sync still runs, just against the
seed. Operators who care about routing-correctness should treat the
`rpc pool WARN` as an alert to investigate the primary, not as a
sync-state error.

## Pool consistency invariants

These are enforced at startup by the doctor and on every status refresh
in the running syncer. Violations are FATAL because they almost always
mean the operator wired the wrong network into one slot:

- **chain_id consistency.** Every reachable endpoint must report the
  same `node_info.network`. If primary says `structstestnet-111` and
  seed says `wrongchain-1`, the doctor refuses with
  `rpc pool FATAL: endpoints disagree on chain_id`.
- **expected chain_id match.** Once the pool's chain_id is known, it's
  compared to the cursor's stored `chain_id` (set on first run). A
  mismatch on resume means someone re-pointed the deployment at a
  different chain and `sync-state` will refuse rather than corrupt the
  existing tables.

## Tunables

- `-poll` / `SYNC_POLL_INTERVAL` (default `3s`) — tip-poll cadence when
  caught up. Lower = faster reaction to new blocks; higher = less load
  on both endpoints.
- The per-endpoint `/status` cache TTL is hard-coded at 5 s. This is
  deliberately short so a primary that just caught up comes back into
  rotation on the next request, not after a long delay. There's no env
  var for it because tuning would risk surprising operators with stale
  routing decisions during reorgs.

## What we explicitly do not do

- **No N-host load distribution.** The pool is preference-ordered, not
  round-robin. Bottleneck is Postgres, not network.
- **No `/net_info` auto-discovery.** Peer rpc_addresses are usually
  loopback-bound or behind a reverse-proxy and can't be trusted blindly.
  Operators curate the URL list explicitly.
- **No light-client / chain-of-hashes validation.** Every URL in the
  pool is assumed operator-trusted. If you ever want to support
  untrusted RPCs (e.g. random `/net_info` peers), that's a separate,
  much larger workstream.

## Deprecation timeline

The old `-rpc` flag and `STRUCTS_RPC_URL` env var are still honored.
They emit a one-line `NOTICE:` at startup pointing operators to the new
flags. They'll stick around at least one minor release after this one
to give the docker-structs-guild deployment a chance to migrate. The
day they're removed, any unmigrated unit will fall back to seed-only
(which still works against the testnet) plus a missing-primary banner.
