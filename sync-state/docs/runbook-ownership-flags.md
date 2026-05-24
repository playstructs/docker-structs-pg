# What sync-state owns (historical reference)

> Related runbooks:
> [`runbook-rpc-failover.md`](runbook-rpc-failover.md) (primary/seed RPC pool),
> [`runbook-verify-and-replay.md`](runbook-verify-and-replay.md) (`sync-state verify` + `reprocess-errors`),
> [`runbook-init-genesis.md`](runbook-init-genesis.md) (genesis import).

This file used to document the per-deployment `SYNC_STATE_OWN_*` flags.
Those flags were removed when the `cache.*` subsystem was retired and
`sync-state` took unconditional ownership of every derived side-effect.

## What sync-state owns now

| Derivation | Replaces dropped trigger | Source file |
|------------|--------------------------|-------------|
| `structs.planet_activity` for struct movement | `planet_activity_struct_movement` on `structs.struct` | [`internal/events/struct.go`](../internal/events/struct.go) |
| `structs.planet_activity` for fleet movement | `planet_activity_fleet_move` on `structs.fleet` | [`internal/events/fleet.go`](../internal/events/fleet.go) |
| `structs.planet_activity` for raid status | `planet_activity_raid_status` on `structs.planet_raid` | [`internal/events/raid.go`](../internal/events/raid.go) |
| `structs.planet_activity` for struct attribute | `planet_activity_struct_attribute` on `structs.struct_attribute` | [`internal/events/struct_attribute.go`](../internal/events/struct_attribute.go) |
| `structs.player_address.guild_id` cascade | `update_address_guild_id` on `structs.player` | [`internal/events/player.go`](../internal/events/player.go) |
| `structs.planet_meta` seeding | `name_planet` on `structs.planet` | [`internal/events/planet.go`](../internal/events/planet.go) |
| `structs.ledger` for infusion fuel delta | `add_infusion_ledger_entry` on `structs.infusion` | [`internal/events/infusion.go`](../internal/events/infusion.go) |
| `structs.ledger` + `structs.defusion` for bank/staking | `cache.PROCESS_BLOCK_LEDGER` (via `cache.TRANSFER_LEDGER_ENTRY` on `cache.blocks`) | [`internal/bank/process.go`](../internal/bank/process.go) |
| `structs.ledger` for genesis bank/staking/ore | docker-structsd's `scripts/indexer-insert-genesis.sh` (Bash + `jq`) | [`internal/genesis/apply.go`](../internal/genesis/apply.go) — see [`runbook-init-genesis.md`](runbook-init-genesis.md) |

The doctor's `cache-era triggers` probe asserts none of these PG
triggers are still enabled — if they are, `sync-state` refuses to
start because every block would double-write.

## Bug fixes baked into the Go ports

These divergences from the dropped SQL triggers are deliberate fixes
for long-standing bugs documented in code comments. They're additive
(more accurate seq, more complete detail, replay-safe block_height) and
nothing downstream is expected to regress:

- **fleet_arrive `fleet_list`** — the SQL trigger assigned the
  recursive CTE result to `old_move_detail` instead of `new_move_detail`,
  so `fleet_arrive` events with `fleet_status='away'` always lost the
  list. Fixed.
- **fleet_depart seq** — the SQL trigger sourced the per-planet seq
  counter from the arrival planet, so multiple departs from the same
  planet collided. Fixed.
- **struct_defense_remove on DELETE** — the SQL trigger's DELETE
  branch referenced `NEW.attribute_type`, which is NULL on DELETE in
  PG, making the entire branch dead code. Fixed to use the parsed
  `attrType`.
- **planet_meta seed** — the SQL trigger inserted `(planet.id, NULL)`
  when the owner's player row had no guild yet, hitting a NOT NULL
  constraint and rolling back the planet insert (observed at h=793782).
  Fixed: skip the seed when `guild_id IS NULL`, then backfill on the
  next `EventPlayer` for that player.
- **infusion ledger block_height** — the SQL trigger read
  `(SELECT height FROM structs.current_block LIMIT 1)`, racing with
  the per-block tx. Fixed: source from `bctx.Height` so replays land
  the correct historical block.

## Running alongside a catching-up node

`sync-state` (without `-one-shot`) does NOT require the upstream
CometBFT node to be fully synced before starting. It polls `/status`
every `SYNC_POLL_INTERVAL` (3 s default) and processes whatever blocks
the node has made available. As the node advances toward chain tip,
`sync-state` follows it block-for-block, sleeping when there's nothing
new and resuming automatically.

What you'll see at startup when primary is catching up but seed is caught up:

```
Connected: chain_id=structstestnet-111 tip=798200 earliest=1 catching_up=false
[WARN] node liveness  catching_up=true at tip=12345 (sync-state will skip this endpoint for blocks above its tip until it catches up)
```

Sync-state indexes against the seed tip (~798k) while local `structsd` catches
up. Block fetches and `tip_height` both track the network until primary reports
`catching_up=false`, then primary becomes the tip source again.

If every endpoint in the pool is catching up (no caught-up seed), you'll see:

```
Connected: chain_id=structstestnet-111 tip=12345 earliest=1 catching_up=true
```

At the tip (rate-limited to once every 30 s) you'll see:

```
at chain-tip h=796800 (sleeping 3s between polls)
```

If only a catching-up node is reachable (no seed fallback), you'll instead see:

```
at node-tip h=12500 (node still catching_up; will track as it advances)
```

Tunables relevant to this mode:

| Flag / env | Default | Notes |
|------------|---------|-------|
| `-poll` / `SYNC_POLL_INTERVAL` | `3s` | Sleep between tip polls when caught up. Lower = faster reaction to new blocks; higher = less RPC load. |
| `-one-shot` / `SYNC_ONE_SHOT` | `false` | If true, exit when caught up to whatever tip the node has NOW. Set ONLY for batch backfills; never for follow-the-tip operation. |
| `-batch` / `SYNC_BATCH_SIZE` | `200` | Max blocks fetched per RPC window. Lower during initial node-catchup helps amortize per-block fixed costs across less work in flight. |

## Canonical schema requirement (post-Phase B)

`sync-state` no longer ALTERs the structs schema at startup. The columns
the Go handlers write — `structs.current_block.{status,lag_blocks,tip_height}`,
`structs.planet_activity.block_height`, and the ten
`structs.stat_*.block_height` columns — now ship as part of canonical
structs-pg via the [`retire-cache.sql`](../sql/retire-cache.sql) Phase B
migration.

The doctor's `canonical schema` probe runs at every startup and FATALs if
any column is missing, so the failure mode is "refuses to start" rather
than "writes for an hour then errors mid-block". The corrective action
when you see the FATAL is to apply `retire-cache.sql` and restart.

## What's still owned by PG triggers

The Phase A6 audit walked every `trigger-*.sql` file under
`references/structs-pg/deploy/` and confirmed that none of the kept
triggers read from the `cache.*` schema — so dropping the schema in
Phase B cannot break them. Sources searched for `FROM cache.` /
`JOIN cache.` / direct cache writes returned zero hits across all 17
kept trigger files.

These remain PG-side and are NOT touched by `sync-state`:

| Trigger | Fires on | What it does | Why it stays |
|---------|----------|-------------|--------------|
| `PLANET_ACTIVITY_NOTIFY` | `structs.planet_activity` AFTER INSERT | `pg_notify('grass', …)` — drives SSE | Reads NEW only |
| `GRID_NOTIFY` | `structs.grid` AFTER INSERT/UPDATE | `pg_notify('grass', …)` | Reads NEW only |
| `INFUSION_NOTIFY`, `INVENTORY_NOTIFY`, `GUILD_NOTIFY`, `PROVIDER_NOTIFY`, `PLAYER_NOTIFY`, `AGREEMENT_NOTIFY`, `CURRENT_BLOCK_NOTIFY` | matching `structs.*` table | `pg_notify('grass', …)` | Reads NEW only |
| `PLAYER_PENDING_MERGE` | `structs.player` AFTER INSERT | DELETEs the matching `player_pending` / `player_internal_pending` row, threads `player_id` into `signer.role` + `structs.player_discord` | Pure structs/signer schema reads |
| `PLAYER_INTERNAL_PENDING` | `structs.player_internal_pending` BEFORE INSERT | Creates a `signer.role` stub | No cache reads |
| `PLAYER_PENDING_JOIN_PROXY` | `structs.player_pending` AFTER INSERT | Calls `signer.CREATE_TRANSACTION` for guild membership | No cache reads |
| `PLAYER_ADDRESS_PENDING_MERGE` | `structs.player_address` AFTER INSERT | Clears the matching `player_address_pending` row | No cache reads |
| `PLAYER_ADDRESS_CASCADE` | `structs.player` AFTER INSERT/UPDATE | Propagates `guild_id` changes to `structs.player_address` and seeds the address row on player insert | Duplicates `playerHandler`'s explicit propagation; harmless (UPDATE no-op due to the WHERE guard) and acts as defense-in-depth |
| `activation_code_cleaner` (pg_cron) | every 59 s | `CALL structs.CLEAN_ACTIVATION_CODE()` — TTLs `structs.player_address_activation_code` | No cache reads |

The Phase B SQL leaves all of these in place.
