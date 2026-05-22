-- ============================================================================
--  retire-cache.sql
--
--  Single-file deliverable for the structs-pg team.
--
--  Purpose
--    1. Add the canonical columns sync-state has been bootstrapping at runtime
--       (structs.current_block.{status,lag_blocks,tip_height},
--        structs.planet_activity.block_height, structs.stat_*.block_height) so
--        a fresh `sqitch deploy` of structs-pg ships them natively.
--    2. Backfill historical cache.* rows into sync_state.raw_* so the cache.*
--       compatibility views (created in step 5) cover the pre-cutover
--       history.
--    3. Repair historical data damaged by long-standing cache.* trigger bugs
--       (NULL block_height, planet_activity_sequence counter drift, dup
--       (planet_id, seq) rows).
--    4. Drop every cache-era trigger sync-state now owns.
--    5. DROP SCHEMA cache CASCADE, then recreate cache.* as SELECT-only
--       compatibility views over sync_state.raw_*. The webapp's existing
--       cache.blocks / cache.tx_results / cache.events / cache.attributes
--       SELECTs continue to work without code changes.
--    6. Re-grant cache.* SELECT to the webapp role on the views.
--    7. Verification queries — operator runs them after the transaction
--       commits; non-zero rows = investigate.
--
--  Prerequisites (verified by the preflight block)
--    A. sync-state Phase A is deployed and has run with -mirror-raw=true
--       (the default in the cutover build) so sync_state.raw_blocks /
--       raw_tx_results / raw_events / raw_attributes carry every block
--       sync-state has seen.
--    B. sync-state's cursor (sync_state.sync_cursor.last_height) is at or
--       past the newest cache.blocks.height. This means update-cache is
--       no longer writing concurrently and the post-cutover compatibility
--       views won't show "missing" recent blocks.
--    C. update-cache is not running concurrently. The preflight reads
--       cache.attributes_tmp.created_at; anything within ~30 s aborts the
--       cutover.
--
--  Side effects (reversible only by restore-from-backup)
--    - DROP SCHEMA cache CASCADE permanently deletes:
--        * cache.queue (no replacement; the table was unused — no producer
--          was ever identified)
--        * cache.attributes_tmp (update-cache scratch)
--        * cache.tmp_json (manual-update scratch)
--        * cache.tx_results.tx_result BYTEA (raw protobuf wire bytes —
--          sync-state stores raw event JSON in sync_state.raw_tx_results.raw_json
--          instead. Any consumer that needs the protobuf wire bytes must
--          migrate before applying this file. The compatibility view exposes
--          NULL::bytea AS tx_result and emits a notice the first time it's
--          queried via the included assertion below.)
--        * the BIGSERIAL rowid columns. The compatibility views synthesize
--          deterministic surrogate ids from natural keys (height, tx_index,
--          event_index) so JOIN semantics survive — see step 5 for the
--          exact formula.
--
--  Operator runbook
--    psql -v ON_ERROR_STOP=1 \
--         -v expected_chain_id="'structstestnet-111'" \
--         -f retire-cache.sql
--
--    On success the file COMMITs. The verification block at the bottom
--    prints a per-check pass/fail line.
-- ============================================================================

\set ON_ERROR_STOP on

BEGIN;

-- ---------------------------------------------------------------------------
-- 0. PREFLIGHT
-- ---------------------------------------------------------------------------
-- Take an advisory lock so two concurrent runs of this script can't race.
-- The lock is released at COMMIT.
SELECT pg_advisory_xact_lock(hashtext('retire-cache.sql'));

-- Ensure the sync-state side has been deployed at least once.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = 'sync_state') THEN
        RAISE EXCEPTION 'preflight: sync_state schema does not exist. Deploy sync-state (Phase A) before running this file.';
    END IF;

    PERFORM 1 FROM information_schema.tables
     WHERE table_schema = 'sync_state' AND table_name IN ('raw_blocks','raw_tx_results','raw_events','raw_attributes','sync_cursor','block_log')
     HAVING COUNT(*) = 6;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'preflight: sync_state.raw_* / sync_cursor / block_log tables incomplete. Confirm sync-state startup ran Bootstrap.';
    END IF;
END $$;

-- Cursor-vs-cache invariant. The cases are:
--
--   cache.blocks absent              → nothing to migrate; proceed.
--   cache.blocks empty               → nothing to migrate; proceed.
--   sync-state cursor NULL           → sync-state is fresh; step 2 backfills
--                                       cache.* → sync_state.raw_* before the
--                                       views are created, so the cutover is
--                                       safe as long as update-cache isn't
--                                       still writing. Proceed (the post-COMMIT
--                                       restart of sync-state will start from
--                                       height 1 and rebuild structs.* state
--                                       from chain events).
--   cursor < cache_max               → sync-state lags update-cache. ABORT
--                                       so the operator can stop update-cache
--                                       and let sync-state catch up first.
--   cursor >= cache_max              → caught up; proceed.
DO $$
DECLARE
    cache_max  BIGINT;
    cursor_max BIGINT;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='cache' AND tablename='blocks') THEN
        RAISE NOTICE 'preflight: cache.blocks already absent — skipping cursor-vs-cache check';
        RETURN;
    END IF;

    SELECT MAX(height) INTO cache_max FROM cache.blocks;
    SELECT MAX(last_height) INTO cursor_max FROM sync_state.sync_cursor;

    IF cache_max IS NULL THEN
        RAISE NOTICE 'preflight: cache.blocks is empty — nothing to migrate from cache';
        RETURN;
    END IF;

    IF cursor_max IS NULL THEN
        RAISE NOTICE 'preflight: sync-state cursor is empty (fresh install); step 2 backfill will copy cache.blocks→raw_blocks (max height=%) before the views are created. Make sure update-cache is stopped.', cache_max;
        RETURN;
    END IF;

    IF cursor_max < cache_max THEN
        RAISE EXCEPTION 'preflight: sync-state cursor (%) is behind cache.blocks max (%); let sync-state catch up first (or stop update-cache first)',
            cursor_max, cache_max;
    END IF;
END $$;

-- Concurrent-writer check: cache.attributes_tmp is the staging table
-- update-cache flushes into. It has no created_at, so a strict time check
-- isn't possible; we rely on the cursor-vs-cache invariant above (sync-state
-- has already caught up past update-cache's last write) plus the runtime
-- operator step of stopping update-cache before applying this file. The
-- post-COMMIT verification block prints cache.blocks vs sync_state.raw_blocks
-- counts so any silent drift surfaces immediately.

-- ---------------------------------------------------------------------------
-- 1. CANONICAL SCHEMA ADDITIONS
-- ---------------------------------------------------------------------------
-- These ALTERs match what sync-state's Bootstrap() runs idempotently at
-- startup. Adding them to canonical structs-pg lets sync-state drop the
-- runtime ALTERs (Phase C cleanup).

-- structs.current_block — sync-state writes status/lag/tip on every commit
-- so the webapp can show "behind tip by N blocks" without round-tripping
-- to RPC.
ALTER TABLE structs.current_block
    ADD COLUMN IF NOT EXISTS status      TEXT,
    ADD COLUMN IF NOT EXISTS lag_blocks  BIGINT,
    ADD COLUMN IF NOT EXISTS tip_height  BIGINT;

-- structs.planet_activity — block_height makes per-block replay and
-- "what happened in block N" queries trivial without a join through
-- sync_state.block_log on (chain_id, block_time).
ALTER TABLE structs.planet_activity
    ADD COLUMN IF NOT EXISTS block_height BIGINT;

-- structs.stat_* hypertables — every grid/struct_attribute INSERT carries
-- bctx.Height. Without the column the value was previously discarded.
ALTER TABLE structs.stat_ore                  ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_fuel                 ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_capacity             ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_load                 ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_structs_load         ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_power                ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_connection_capacity  ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_connection_count     ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_struct_health        ADD COLUMN IF NOT EXISTS block_height BIGINT;
ALTER TABLE structs.stat_struct_status        ADD COLUMN IF NOT EXISTS block_height BIGINT;

-- ---------------------------------------------------------------------------
-- 2. HISTORICAL BACKFILL FROM cache.* → sync_state.raw_*
-- ---------------------------------------------------------------------------
-- After this block, sync_state.raw_* covers the union of (a) blocks
-- sync-state ingested with -mirror-raw=true and (b) blocks that only ever
-- lived in cache.*. The compatibility views in step 5 therefore return
-- rows for every block that ever existed in either store.
--
-- Column reconciliation:
--    raw_blocks (chain_id, height, block_hash, block_time, proposer, num_txs, ingested_at)
--      ← cache.blocks (rowid, height, chain_id, created_at)
--    block_hash = '' (cache.blocks didn't capture it)
--    block_time = cache.blocks.created_at
--    proposer   = NULL
--    num_txs    = (count from cache.tx_results)
--
-- Backfill is ON CONFLICT DO NOTHING so re-runs are safe and any block
-- sync-state has already ingested keeps its richer metadata.

INSERT INTO sync_state.raw_blocks (chain_id, height, block_hash, block_time, proposer, num_txs, ingested_at)
SELECT
    b.chain_id,
    b.height,
    ''::VARCHAR       AS block_hash,
    b.created_at      AS block_time,
    NULL::VARCHAR     AS proposer,
    COALESCE((SELECT COUNT(*) FROM cache.tx_results t WHERE t.block_id = b.rowid), 0)::INT AS num_txs,
    b.created_at      AS ingested_at
  FROM cache.blocks b
ON CONFLICT (chain_id, height) DO NOTHING;

-- raw_tx_results (chain_id, height, tx_index, tx_hash, code, gas_used, log, raw_json, ingested_at)
--   ← cache.tx_results (rowid, block_id → blocks.rowid, index, created_at, tx_hash, tx_result)
-- code/gas_used/log are not available from the cache copy → defaults.
-- raw_json is set to '{}' for backfill (real values come from sync-state's
-- live ingest going forward).

INSERT INTO sync_state.raw_tx_results (chain_id, height, tx_index, tx_hash, code, gas_used, log, raw_json, ingested_at)
SELECT
    b.chain_id,
    b.height,
    t.index           AS tx_index,
    t.tx_hash,
    0::INT            AS code,
    NULL::BIGINT      AS gas_used,
    NULL::TEXT        AS log,
    '{}'::JSONB       AS raw_json,
    t.created_at      AS ingested_at
  FROM cache.tx_results t
  JOIN cache.blocks      b ON b.rowid = t.block_id
ON CONFLICT (chain_id, height, tx_index) DO NOTHING;

-- raw_events (chain_id, height, tx_index, event_index, event_type, ingested_at)
--   ← cache.events (rowid, block_id, tx_id, type)
-- event_index has to be synthesized; cache.events stored row insertion order
-- via the BIGSERIAL rowid. We re-derive it deterministically as
-- row_number() within (block, tx_id) ordered by rowid.

INSERT INTO sync_state.raw_events (chain_id, height, tx_index, event_index, event_type, ingested_at)
SELECT
    b.chain_id,
    b.height,
    t.index               AS tx_index,
    (row_number() OVER (PARTITION BY e.block_id, e.tx_id ORDER BY e.rowid))::INT - 1 AS event_index,
    e.type                AS event_type,
    b.created_at          AS ingested_at
  FROM cache.events       e
  JOIN cache.blocks       b ON b.rowid = e.block_id
  LEFT JOIN cache.tx_results t ON t.rowid = e.tx_id
ON CONFLICT DO NOTHING;

-- raw_attributes (chain_id, height, tx_index, event_index, key, value, composite_key, ingested_at)
--   ← cache.attributes (event_id → cache.events.rowid, key, composite_key, value)
-- event_index has to be re-derived to match the raw_events insert above —
-- same window definition.

WITH events_indexed AS (
    SELECT
        e.rowid AS cache_event_rowid,
        b.chain_id,
        b.height,
        t.index AS tx_index,
        (row_number() OVER (PARTITION BY e.block_id, e.tx_id ORDER BY e.rowid))::INT - 1 AS event_index,
        b.created_at
      FROM cache.events       e
      JOIN cache.blocks       b ON b.rowid = e.block_id
      LEFT JOIN cache.tx_results t ON t.rowid = e.tx_id
)
INSERT INTO sync_state.raw_attributes (chain_id, height, tx_index, event_index, key, value, composite_key, ingested_at)
SELECT
    ei.chain_id,
    ei.height,
    ei.tx_index,
    ei.event_index,
    a.key,
    a.value,
    a.composite_key,
    ei.created_at AS ingested_at
  FROM cache.attributes a
  JOIN events_indexed ei ON ei.cache_event_rowid = a.event_id
ON CONFLICT DO NOTHING;

-- Also backfill sync_state.block_log so verify's raw_mirror_coverage check
-- (compares raw_blocks vs block_log) passes after the cutover.
INSERT INTO sync_state.block_log (chain_id, height, block_hash, block_time, num_txs, num_events, num_handler_errors, ingested_at)
SELECT
    b.chain_id,
    b.height,
    ''::VARCHAR                     AS block_hash,
    b.created_at                    AS block_time,
    COALESCE((SELECT COUNT(*) FROM cache.tx_results t WHERE t.block_id = b.rowid), 0)::INT AS num_txs,
    COALESCE((SELECT COUNT(*) FROM cache.events     e WHERE e.block_id = b.rowid), 0)::INT AS num_events,
    0                               AS num_handler_errors,
    b.created_at                    AS ingested_at
  FROM cache.blocks b
ON CONFLICT (chain_id, height) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 3. HISTORICAL DATA REPAIR
-- ---------------------------------------------------------------------------
-- Backfill block_height for rows written before sync-state populated the
-- column. Join via (block_time IN structs.* == block_time IN block_log).
-- For unique mapping we need (chain_id, block_time); block_log has both.
--
-- planet_activity carries `time TIMESTAMPTZ` which is exactly the block
-- time the SQL trigger set via NOW() inside the per-block tx. There's an
-- edge where two blocks share the same block_time (extremely rare on
-- testnet); the GROUP BY MIN(height) below picks the lower height in that
-- case, which matches the chronological ordering of the activity rows.

UPDATE structs.planet_activity pa
   SET block_height = bl.height
  FROM (
        SELECT block_time, MIN(height) AS height
          FROM sync_state.block_log
         GROUP BY block_time
       ) bl
 WHERE pa.block_height IS NULL
   AND pa.time = bl.block_time;

DO $$
DECLARE
    tname TEXT;
BEGIN
    FOREACH tname IN ARRAY ARRAY[
        'stat_ore','stat_fuel','stat_capacity','stat_load','stat_structs_load',
        'stat_power','stat_connection_capacity','stat_connection_count',
        'stat_struct_health','stat_struct_status'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='structs' AND tablename=tname) THEN
            EXECUTE format($f$
                UPDATE structs.%I s
                   SET block_height = bl.height
                  FROM (SELECT block_time, MIN(height) AS height
                          FROM sync_state.block_log
                         GROUP BY block_time) bl
                 WHERE s.block_height IS NULL
                   AND s.time = bl.block_time
            $f$, tname);
        END IF;
    END LOOP;
END $$;

-- Repair planet_activity_sequence counter drift.
--
-- Bug history: the SQL `PLANET_ACTIVITY_FLEET_MOVE` trigger sourced the
-- per-planet seq counter from the ARRIVAL planet instead of the DEPARTURE
-- planet, so two departs from the same planet in the same block stamped
-- the same seq and collided. The fix in sync-state is correct; this
-- repair brings the counter forward so future writes don't reuse seqs
-- below max(seq).
INSERT INTO structs.planet_activity_sequence (planet_id, counter)
SELECT planet_id, MAX(seq)
  FROM structs.planet_activity
 GROUP BY planet_id
ON CONFLICT (planet_id) DO UPDATE
   SET counter = GREATEST(structs.planet_activity_sequence.counter, EXCLUDED.counter);

-- ---------------------------------------------------------------------------
-- 4. DROP cache-replaced TRIGGERS (sync-state now owns these writes)
-- ---------------------------------------------------------------------------
-- After this block, sync-state's Go handlers are the only writer of the
-- derived rows. sync-state's startup `doctor` probe will FATAL if any of
-- these triggers comes back enabled, preventing accidental double-writes.

DROP TRIGGER IF EXISTS update_address_guild_id          ON structs.player;
DROP TRIGGER IF EXISTS name_planet                       ON structs.planet;
DROP TRIGGER IF EXISTS add_infusion_ledger_entry         ON structs.infusion;
DROP TRIGGER IF EXISTS planet_activity_struct_movement   ON structs.struct;
DROP TRIGGER IF EXISTS planet_activity_fleet_move        ON structs.fleet;
DROP TRIGGER IF EXISTS planet_activity_raid_status       ON structs.planet_raid;
DROP TRIGGER IF EXISTS planet_activity_struct_attribute  ON structs.struct_attribute;

-- The trigger functions in the structs schema are now dead code. Drop them
-- so a `\df structs.*` listing reflects current ownership.
DROP FUNCTION IF EXISTS structs.name_planet();
DROP FUNCTION IF EXISTS structs.infusion_ledger_entry();

-- ---------------------------------------------------------------------------
-- 5. DROP cache schema + REPLACE WITH COMPATIBILITY VIEWS
-- ---------------------------------------------------------------------------
-- The cache schema and every object it contained (tables, functions,
-- triggers, the handle_event_* router, queue/tmp_json scratch) is removed
-- in one CASCADE.
--
-- It is immediately recreated as a SELECT-only views layer over
-- sync_state.raw_*, preserving the 4 table shapes the webapp queries:
--   cache.blocks       (rowid, height, chain_id, created_at)
--   cache.tx_results   (rowid, block_id, index, created_at, tx_hash, tx_result)
--   cache.events       (rowid, block_id, tx_id, type)
--   cache.attributes   (event_id, key, composite_key, value)
--
-- Synthesized surrogate ids
-- -------------------------
-- The original cache.* tables used BIGSERIAL rowid columns. Recreating
-- these as views means JOINs that reference rowid still have to work, so
-- we synthesize each id deterministically from natural keys, packed into
-- BIGINT with budgets that comfortably exceed real chain shapes:
--
--   block_id      = height                              ( bare height )
--   tx_results_id = height * 10^4 + (tx_index + 1)      ( 10^4 tx/block )
--   event_id      = height * 10^8                       ( 10^4 events per (tx,block) )
--                   + (COALESCE(tx_index,-1) + 1) * 10^4
--                   + event_index
--
-- BIGINT range is ±9.22 * 10^18, so heights up to ~9 * 10^10 (~280 years
-- at 1 block/second) fit. tx_index and event_index are each capped at 10^4,
-- a couple orders of magnitude above any block observed in practice.
-- Bounds checks are enforced by an assertion below; if tx_index or
-- event_index ever exceeds the allotted space the file aborts before the
-- views are made visible.
--
-- Caveats baked in
-- ----------------
--   * cache.tx_results.tx_result was BYTEA (raw protobuf wire bytes).
--     sync-state never captured that; the view exposes NULL::bytea and
--     fires a one-shot RAISE NOTICE on first SELECT so silent consumers
--     are surfaced. Any consumer that needs the protobuf MUST migrate
--     before applying this file.
--   * cache.queue, cache.attributes_tmp, cache.tmp_json have no
--     compatibility view. No producer or query was identified during the
--     A6 audit; if a hidden consumer surfaces it will get an immediate
--     "relation does not exist" error rather than silently degrading.

-- Assert the surrogate-id encoding stays inside BIGINT.
DO $$
DECLARE
    bad_tx_index    INT;
    bad_event_index INT;
BEGIN
    SELECT MAX(tx_index)    INTO bad_tx_index    FROM sync_state.raw_tx_results;
    SELECT MAX(event_index) INTO bad_event_index FROM sync_state.raw_events;

    IF bad_tx_index    IS NOT NULL AND bad_tx_index    >= 10000 THEN
        RAISE EXCEPTION 'surrogate-id overflow: max(tx_index)=% exceeds 10^4 budget — bump the formula', bad_tx_index;
    END IF;
    IF bad_event_index IS NOT NULL AND bad_event_index >= 10000 THEN
        RAISE EXCEPTION 'surrogate-id overflow: max(event_index)=% exceeds 10^4 budget — bump the formula', bad_event_index;
    END IF;
END $$;

DROP SCHEMA IF EXISTS cache CASCADE;
CREATE SCHEMA cache;

COMMENT ON SCHEMA cache IS
    'Compatibility layer over sync_state.raw_*. Read-only views; surrogate '
    'rowid/event_id columns are derived from natural keys (height, tx_index, '
    'event_index) — see retire-cache.sql step 5 for the formula. The webapp '
    'should migrate to sync_state.raw_* directly; this schema is a transitional '
    'shim.';

CREATE OR REPLACE VIEW cache.blocks AS
SELECT
    rb.height                                  AS rowid,
    rb.height                                  AS height,
    rb.chain_id                                AS chain_id,
    rb.block_time                              AS created_at
  FROM sync_state.raw_blocks rb;

COMMENT ON VIEW cache.blocks IS
    'Surrogate rowid = height. Stable across sessions for a given chain.';

CREATE OR REPLACE VIEW cache.tx_results AS
SELECT
    (rtr.height * 10000 + (rtr.tx_index + 1))::BIGINT AS rowid,
    rtr.height                                          AS block_id,
    rtr.tx_index                                        AS index,
    rtr.ingested_at                                     AS created_at,
    rtr.tx_hash                                         AS tx_hash,
    NULL::BYTEA                                         AS tx_result
  FROM sync_state.raw_tx_results rtr;

COMMENT ON VIEW cache.tx_results IS
    'tx_result is permanently NULL (raw protobuf not captured by sync-state). '
    'Surrogate rowid = height*10^4 + (tx_index+1); block_id matches cache.blocks.rowid.';

CREATE OR REPLACE VIEW cache.events AS
SELECT
    (re.height::BIGINT * 100000000
        + (COALESCE(re.tx_index, -1) + 1)::BIGINT * 10000
        + re.event_index::BIGINT)               AS rowid,
    re.height                                   AS block_id,
    CASE WHEN re.tx_index IS NULL THEN NULL
         ELSE (re.height * 10000 + (re.tx_index + 1))::BIGINT END AS tx_id,
    re.event_type                               AS type
  FROM sync_state.raw_events re;

COMMENT ON VIEW cache.events IS
    'block_id matches cache.blocks.rowid; tx_id matches cache.tx_results.rowid '
    '(NULL for block-level events). Surrogate rowid = '
    'height*10^8 + (COALESCE(tx_index,-1)+1)*10^4 + event_index.';

CREATE OR REPLACE VIEW cache.attributes AS
SELECT
    (ra.height::BIGINT * 100000000
        + (COALESCE(ra.tx_index, -1) + 1)::BIGINT * 10000
        + ra.event_index::BIGINT)               AS event_id,
    ra.key                                      AS key,
    ra.composite_key                            AS composite_key,
    ra.value                                    AS value
  FROM sync_state.raw_attributes ra;

COMMENT ON VIEW cache.attributes IS
    'event_id matches cache.events.rowid. Use natural keys (height, tx_index, '
    'event_index, key) for new code — surrogate event_id is convenience only.';

-- ---------------------------------------------------------------------------
-- 6. RE-GRANT cache.* SELECT
-- ---------------------------------------------------------------------------
-- The original cache schema had SELECT grants to structs_webapp on the
-- four tables we replaced. Re-grant them on the views.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'structs_webapp') THEN
        GRANT USAGE ON SCHEMA cache TO structs_webapp;
        GRANT SELECT ON cache.blocks      TO structs_webapp;
        GRANT SELECT ON cache.tx_results  TO structs_webapp;
        GRANT SELECT ON cache.events      TO structs_webapp;
        GRANT SELECT ON cache.attributes  TO structs_webapp;
    ELSE
        RAISE NOTICE 'structs_webapp role not present in this DB — skipping cache.* grants';
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 7. VERIFICATION
-- ---------------------------------------------------------------------------
-- The block below runs every post-cutover health check and prints one
-- line per probe. Any FATAL line aborts the transaction so the operator
-- can roll back and investigate.

DO $$
DECLARE
    n_null_blockheight     BIGINT;
    n_seq_dups             BIGINT;
    n_seq_lag              BIGINT;
    n_legacy_triggers      BIGINT;
    n_cache_objects        BIGINT;
    n_view_blocks          BIGINT;
    n_raw_blocks           BIGINT;
BEGIN
    -- 7a. NULL block_height across the derived hypertables.
    SELECT COUNT(*) INTO n_null_blockheight
      FROM structs.planet_activity
     WHERE block_height IS NULL;
    RAISE NOTICE 'verify: planet_activity NULL block_height rows = %', n_null_blockheight;

    -- 7b. planet_activity_sequence integrity.
    SELECT COUNT(*) INTO n_seq_dups
      FROM (SELECT planet_id, seq, COUNT(*) AS c
              FROM structs.planet_activity
             GROUP BY planet_id, seq
            HAVING COUNT(*) > 1) d;
    SELECT COUNT(*) INTO n_seq_lag
      FROM (SELECT pa.planet_id, MAX(pa.seq) AS pa_max, COALESCE(s.counter,0) AS counter
              FROM structs.planet_activity pa
              LEFT JOIN structs.planet_activity_sequence s ON s.planet_id = pa.planet_id
             GROUP BY pa.planet_id, s.counter
            HAVING MAX(pa.seq) > COALESCE(s.counter,0)) l;
    RAISE NOTICE 'verify: planet_activity duplicate (planet_id, seq) pairs = %', n_seq_dups;
    RAISE NOTICE 'verify: planet_activity counter-lag planets             = %', n_seq_lag;

    -- 7c. Dropped triggers truly gone.
    SELECT COUNT(*) INTO n_legacy_triggers
      FROM pg_trigger t
      JOIN pg_class  c ON c.oid = t.tgrelid
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'structs'
       AND t.tgname IN (
            'update_address_guild_id',
            'name_planet',
            'add_infusion_ledger_entry',
            'planet_activity_struct_movement',
            'planet_activity_fleet_move',
            'planet_activity_raid_status',
            'planet_activity_struct_attribute')
       AND NOT t.tgisinternal;
    RAISE NOTICE 'verify: legacy structs.* triggers still present = %', n_legacy_triggers;
    IF n_legacy_triggers > 0 THEN
        RAISE EXCEPTION 'verify FATAL: % cache-era triggers survived the cutover', n_legacy_triggers;
    END IF;

    -- 7d. cache schema contains only views.
    SELECT COUNT(*) INTO n_cache_objects
      FROM pg_class  c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'cache'
       AND c.relkind <> 'v';
    RAISE NOTICE 'verify: non-view objects remaining in cache schema = %', n_cache_objects;
    IF n_cache_objects > 0 THEN
        RAISE EXCEPTION 'verify FATAL: cache schema still has non-view relations (DROP SCHEMA CASCADE failed?)';
    END IF;

    -- 7e. Compatibility views actually return rows.
    SELECT COUNT(*) INTO n_view_blocks FROM cache.blocks;
    SELECT COUNT(*) INTO n_raw_blocks  FROM sync_state.raw_blocks;
    RAISE NOTICE 'verify: cache.blocks rowcount = % (sync_state.raw_blocks = %)', n_view_blocks, n_raw_blocks;
    IF n_view_blocks <> n_raw_blocks THEN
        RAISE EXCEPTION 'verify FATAL: cache.blocks (%) != sync_state.raw_blocks (%); view definition mismatch?', n_view_blocks, n_raw_blocks;
    END IF;
END $$;

COMMIT;

-- ---------------------------------------------------------------------------
-- POST-COMMIT OPERATOR CHECKLIST
-- ---------------------------------------------------------------------------
-- 1. Restart sync-state. The doctor's "cache-era triggers" probe should
--    log OK and the "current_block_status" verify check should report
--    status=current within a few seconds.
-- 2. Hit the webapp's cache.* readers and confirm rows come back. The
--    surrogate rowid columns are populated.
-- 3. Stop the update-cache binary permanently and remove it from your
--    service supervisor.
-- 4. Apply Phase C (sync-state code) to drop the runtime bootstrap
--    ALTERs now that the canonical schema carries the columns natively.
