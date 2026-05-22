#!/usr/bin/env bash

# Sync the local Structs DB block-by-block from a CometBFT RPC endpoint.
#
# Unlike update-cache (which snapshots current entity state via the LCD), this
# tool walks blocks in order and routes events directly to structs.* via
# in-process Go handlers (replacing the legacy cache.* SQL triggers).
#
# At startup the tool runs:
#   - bootstrap (idempotent: CREATE SCHEMA sync_state + tables + ALTERs)
#   - doctor    (RPC node compatibility, trigger-vs-flag matrix, writer lock)
# then enters the ingest loop.
#
# Subcommands (default = ingest):
#   sync-state                 -- tip-follow ingest
#   sync-state ingest          -- explicit alias
#   sync-state bootstrap       -- idempotent schema setup, then exit
#   sync-state doctor          -- run all checks and exit
#   sync-state list-handlers   -- print the registered event handlers
#
# Configuration (env vars; all optional):
#
#   --- RPC + DB ---
#   STRUCTS_RPC_URL           default http://structsd:26657
#   DATABASE_URL              default built from PG* env vars
#   PGSSLMODE                 default disable; set to require for the SSL listener
#   SYNC_STATE_DB_MAX_CONNS   default 4 (budget alongside update-cache + webapp)
#
#   --- ingest knobs ---
#   SYNC_START_HEIGHT         default 0 (resume from sync_state.sync_cursor,
#                              clamped to node's earliest_block_height)
#   SYNC_STOP_HEIGHT          default 0 (follow tip forever)
#   SYNC_BATCH_SIZE           default 200
#   SYNC_PARALLELISM          default 8
#   SYNC_POLL_INTERVAL        default 3s   (private RPC); 30s recommended for public RPC
#   SYNC_HTTP_TIMEOUT         default 30s
#   SYNC_HTTP_RETRIES         default 5
#   SYNC_LOG_EVERY            default 100
#   SYNC_ONE_SHOT             default false (true => exit when caught up)
#
#   --- safety / ownership ---
#   SYNC_STATE_MIRROR_RAW         default false (true => mirror raw rows to
#                                    sync_state.raw_*; dev convenience, ~2x WAL)
#   SYNC_STATE_STRICT_UNKNOWN     default false (true => unknown composite_keys
#                                    are fatal; useful in CI)
#   SYNC_STATE_SKIP_DOCTOR        default false (skip RPC + trigger probes;
#                                    writer lock is always acquired)
#
#   SYNC_STATE_OWN_DERIVATIONS    default false (Go owns planet_activity /
#                                    address-guild / struct-status derivations)
#   SYNC_STATE_OWN_INFUSION_LEDGER  default false (Go owns infusion ledger entries)
#   SYNC_STATE_OWN_PLANET_META    default false (Go owns planet_meta seeding)
#
# Upstream node requirements (verified by doctor):
#   app.toml      pruning = "nothing", min-retain-blocks = 0
#   config.toml   [storage] discard_abci_responses = false
#                 [tx_index] indexer = "kv"   (or "psql"; not "null")
#
# Extra args are passed through to the binary.

echo "Syncing Structs DB block-by-block from CometBFT RPC"
exec /usr/local/bin/sync-state "$@"
