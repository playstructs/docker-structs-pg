-- ============================================================================
--  bootstrap.sql
--
--  Single-file canonical DDL of everything sync-state's runtime Bootstrap()
--  currently creates. Deliverable to the structs-pg team for porting into
--  sqitch — once a sqitch change ships these objects, sync-state's
--  Bootstrap() will be reduced to a read-only doctor probe and this file
--  becomes the authoritative source of shape.
--
--  Scope (what Bootstrap touches today)
--    - CREATE SCHEMA sync_state
--    - 10 tables under sync_state:
--        sync_cursor, handler_error_log, block_log, verification_report,
--        raw_blocks, raw_tx_results, raw_events, raw_attributes,
--        unknown_event_log, genesis_log
--    - 1 sequence (implicit, from handler_error_log.id BIGSERIAL):
--        handler_error_log_id_seq
--    - 7 primary-key constraints (5 single-column, 2 composite)
--    - 6 secondary indexes (3 partial, 3 plain)
--    - 1 idempotent ALTER (severity column on handler_error_log — included
--      in the CREATE for fresh DBs, ALTER ADD COLUMN IF NOT EXISTS for the
--      forward-migration of pre-severity deployments)
--
--  Out of scope (NOT in Bootstrap; do not port from here)
--    - Hypertable conversion (none of the sync_state tables are hypertables)
--    - Grants / roles (sync-state owns the schema as its own DB user;
--      grants to structs_webapp / structs_indexer are owned by
--      role-structs-webapp.sql / role-structs-indexer.sql today, and any
--      future grants on sync_state.* should land in those files)
--    - Functions, triggers, comments (none — this is a pure data schema)
--    - structs.* canonical columns (current_block.{status,lag_blocks,
--      tip_height}, planet_activity.block_height, stat_*.block_height) —
--      formerly added by Bootstrap, now owned by sync-state/sql/retire-cache.sql
--      step 1. After this file is ported to sqitch, retire-cache.sql step 1
--      will also be portable into a sqitch change; the sync-state runtime
--      side already does not add those columns anymore.
--
--  Idempotency
--    Every statement uses IF NOT EXISTS / ADD COLUMN IF NOT EXISTS so this
--    file is safe to apply against an already-bootstrapped database.
--    Re-derived directly from sync-state/internal/db/bootstrap.go —
--    pg_dump --schema=sync_state of a freshly-bootstrapped DB produces
--    an equivalent shape (modulo the BIGSERIAL desugaring into an explicit
--    sequence + DEFAULT nextval — pg_dump output kept at the bottom of
--    this file under "REFERENCE: pg_dump output").
-- ============================================================================

-- ---------------------------------------------------------------------------
-- schema
-- ---------------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS sync_state;

-- ---------------------------------------------------------------------------
-- sync_state.sync_cursor
--
-- One row per chain_id. The block-by-block ingest reads/writes this row
-- on every commit (in bulk mode: once per window). It is the canonical
-- "what height have we processed through?" pointer.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state.sync_cursor (
    chain_id          VARCHAR PRIMARY KEY,
    last_height       BIGINT NOT NULL,
    last_block_hash   VARCHAR,
    last_block_time   TIMESTAMPTZ,
    status            TEXT,
    lag_blocks        BIGINT,
    tip_height        BIGINT,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- sync_state.handler_error_log
--
-- Append-only log of every per-event handler error. Operators query this
-- to find handler bugs after a bulk replay. severity is one of
-- {'error','warn','info'} — see internal/events/handler.go for the policy.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state.handler_error_log (
    id                BIGSERIAL PRIMARY KEY,
    chain_id          VARCHAR NOT NULL,
    height            BIGINT NOT NULL,
    tx_index          INT,
    msg_index         INT,
    event_index       INT,
    composite_key     VARCHAR NOT NULL,
    payload           JSONB,
    error             TEXT NOT NULL,
    stack             TEXT,
    severity          TEXT NOT NULL DEFAULT 'error',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at       TIMESTAMPTZ,
    resolved_by       TEXT
);

-- Forward-migration for pre-severity deployments (no-op on fresh DBs since
-- the CREATE above already has the column).
ALTER TABLE sync_state.handler_error_log
    ADD COLUMN IF NOT EXISTS severity TEXT NOT NULL DEFAULT 'error';

CREATE INDEX IF NOT EXISTS handler_error_log_unresolved_idx
    ON sync_state.handler_error_log (chain_id, composite_key, created_at)
    WHERE resolved_at IS NULL;

-- Severity-aware index for the startup banner + verify subcommand
-- (both want fast SELECT severity, COUNT(*) ... WHERE resolved IS NULL).
CREATE INDEX IF NOT EXISTS handler_error_log_severity_unresolved_idx
    ON sync_state.handler_error_log (chain_id, severity)
    WHERE resolved_at IS NULL;

-- ---------------------------------------------------------------------------
-- sync_state.block_log
--
-- One row per ingested block. num_handler_errors lets operators see the
-- error density without aggregating handler_error_log.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state.block_log (
    chain_id            VARCHAR NOT NULL,
    height              BIGINT NOT NULL,
    block_hash          VARCHAR NOT NULL,
    block_time          TIMESTAMPTZ NOT NULL,
    num_txs             INT NOT NULL,
    num_events          INT NOT NULL,
    num_handler_errors  INT NOT NULL DEFAULT 0,
    ingested_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, height)
);

-- ---------------------------------------------------------------------------
-- sync_state.verification_report
--
-- Per-run output of `sync-state verify`. status is one of
-- {'pass','warn','info','fail','skip'}; one row per (run_id, scope[, height,
-- composite_key]). Append-only; runs are distinguished by run_id (UUID).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state.verification_report (
    run_id        UUID NOT NULL,
    scope         TEXT NOT NULL,
    height        BIGINT,
    composite_key TEXT,
    expected      JSONB,
    actual        JSONB,
    status        TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- sync_state.raw_* — raw mirror tables
--
-- Always CREATED, written only when SYNC_STATE_MIRROR_RAW=true (or
-- `-mirror-raw=true`). The cache.* compatibility views created by
-- retire-cache.sql read from these tables; running without mirror-raw
-- means cache.blocks / cache.tx_results / cache.events / cache.attributes
-- show only the rows ingested while mirror-raw was on.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state.raw_blocks (
    chain_id     VARCHAR NOT NULL,
    height       BIGINT NOT NULL,
    block_hash   VARCHAR NOT NULL,
    block_time   TIMESTAMPTZ NOT NULL,
    proposer     VARCHAR,
    num_txs      INT NOT NULL,
    ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, height)
);

CREATE TABLE IF NOT EXISTS sync_state.raw_tx_results (
    chain_id     VARCHAR NOT NULL,
    height       BIGINT NOT NULL,
    tx_index     INT NOT NULL,
    tx_hash      VARCHAR NOT NULL,
    code         INT NOT NULL,
    gas_used     BIGINT,
    log          TEXT,
    raw_json     JSONB NOT NULL,
    ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, height, tx_index)
);

-- raw_events / raw_attributes have NO primary key by design — the chain can
-- emit duplicate (composite_key, value) pairs in a single block and we keep
-- every emission, ordered by event_index. The compatibility views in
-- retire-cache.sql synthesize a deterministic surrogate id from
-- (height, tx_index, event_index) — see that file's step 5 formula.
CREATE TABLE IF NOT EXISTS sync_state.raw_events (
    chain_id     VARCHAR NOT NULL,
    height       BIGINT NOT NULL,
    tx_index     INT,
    event_index  INT NOT NULL,
    event_type   VARCHAR NOT NULL,
    ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS raw_events_height_idx
    ON sync_state.raw_events (chain_id, height);

CREATE TABLE IF NOT EXISTS sync_state.raw_attributes (
    chain_id      VARCHAR NOT NULL,
    height        BIGINT NOT NULL,
    tx_index      INT,
    event_index   INT NOT NULL,
    key           VARCHAR NOT NULL,
    value         TEXT,
    composite_key VARCHAR NOT NULL,
    ingested_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS raw_attributes_height_idx
    ON sync_state.raw_attributes (chain_id, height);
CREATE INDEX IF NOT EXISTS raw_attributes_composite_key_idx
    ON sync_state.raw_attributes (composite_key, height);

-- ---------------------------------------------------------------------------
-- sync_state.unknown_event_log
--
-- One row per distinct composite_key (event_type.attribute_key) that the
-- router did NOT have a registered handler for. Acts as a coverage
-- dashboard for "are we processing every attribute the chain emits?" —
-- operators query this table at any time to see what's been skipped and
-- decide whether a new handler is needed.
--
-- count / first_seen_height / last_seen_height accumulate monotonically
-- across ingest runs (UPSERT with GREATEST / LEAST). last_payload holds
-- the most-recent raw attribute value as a JSON literal so operators can
-- pivot directly into sync_state.raw_attributes for full samples.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state.unknown_event_log (
    chain_id           VARCHAR NOT NULL,
    composite_key      VARCHAR NOT NULL,
    count              BIGINT  NOT NULL DEFAULT 0,
    first_seen_height  BIGINT  NOT NULL,
    last_seen_height   BIGINT  NOT NULL,
    first_seen_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_payload       JSONB,
    PRIMARY KEY (chain_id, composite_key)
);
CREATE INDEX IF NOT EXISTS unknown_event_log_count_idx
    ON sync_state.unknown_event_log (chain_id, count DESC);

-- ---------------------------------------------------------------------------
-- sync_state.genesis_log
--
-- One row per (chain_id) that has had its genesis JSON applied to
-- structs.ledger (action='genesis'). Acts as the idempotency guard for
-- `sync-state init-genesis`: ingest auto-applies on the first start
-- from height 1 when this row is missing, and the verify check FAILs
-- loudly when cursor>0 but genesis was never loaded (which is what
-- produces the false-alarm "negative balance" rows seen during the
-- pre-genesis-import test runs — staking debits with no matching
-- genesis credits).
--
-- source is "rpc:<url>" or "file:<path>" so the post-mortem trail is
-- obvious. sha256 is over the raw genesis JSON bytes (pre-parse) so a
-- reapply against a tampered genesis is detectable. rows_per_section
-- is a small JSONB ({"bank":138,"delegations":40,...}) the verify
-- runner can surface without re-parsing genesis.
--
-- Cross-check invariant (enforced by `sync-state verify` and by
-- retire-cache.sql's step 7g probe):
--   total_rows MUST equal COUNT(*) FROM structs.ledger WHERE action='genesis'
--
-- Divergence means someone deleted structs.ledger genesis rows out from
-- under the genesis_log entry; both gates will FAIL with a clear pointer
-- to `sync-state init-genesis -force` as the fix.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state.genesis_log (
    chain_id           VARCHAR PRIMARY KEY,
    applied_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    source             TEXT NOT NULL,
    genesis_time       TIMESTAMPTZ NOT NULL,
    sha256             VARCHAR(64) NOT NULL,
    rows_per_section   JSONB NOT NULL,
    total_rows         BIGINT NOT NULL
);

-- ============================================================================
-- REFERENCE: pg_dump --schema-only --schema=sync_state output
--
-- Below is the canonical pg_dump produced by applying every CREATE above
-- to a fresh database and then running:
--
--   pg_dump --schema-only --no-owner --no-acl --schema=sync_state DBNAME
--
-- Use this as the spec for the sqitch port — anything the sqitch deploy
-- creates that does not match a statement below is a deviation. The
-- BIGSERIAL above desugars into the explicit sequence + DEFAULT nextval
-- you see here; both forms produce identical pg_dump output.
-- ============================================================================
--
--   CREATE SCHEMA sync_state;
--
--   CREATE TABLE sync_state.block_log (
--       chain_id character varying NOT NULL,
--       height bigint NOT NULL,
--       block_hash character varying NOT NULL,
--       block_time timestamp with time zone NOT NULL,
--       num_txs integer NOT NULL,
--       num_events integer NOT NULL,
--       num_handler_errors integer DEFAULT 0 NOT NULL,
--       ingested_at timestamp with time zone DEFAULT now() NOT NULL
--   );
--
--   CREATE TABLE sync_state.genesis_log (
--       chain_id character varying NOT NULL,
--       applied_at timestamp with time zone DEFAULT now() NOT NULL,
--       source text NOT NULL,
--       genesis_time timestamp with time zone NOT NULL,
--       sha256 character varying(64) NOT NULL,
--       rows_per_section jsonb NOT NULL,
--       total_rows bigint NOT NULL
--   );
--
--   CREATE TABLE sync_state.handler_error_log (
--       id bigint NOT NULL,
--       chain_id character varying NOT NULL,
--       height bigint NOT NULL,
--       tx_index integer,
--       msg_index integer,
--       event_index integer,
--       composite_key character varying NOT NULL,
--       payload jsonb,
--       error text NOT NULL,
--       stack text,
--       severity text DEFAULT 'error'::text NOT NULL,
--       created_at timestamp with time zone DEFAULT now() NOT NULL,
--       resolved_at timestamp with time zone,
--       resolved_by text
--   );
--
--   CREATE SEQUENCE sync_state.handler_error_log_id_seq
--       START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
--   ALTER SEQUENCE sync_state.handler_error_log_id_seq
--       OWNED BY sync_state.handler_error_log.id;
--   ALTER TABLE ONLY sync_state.handler_error_log
--       ALTER COLUMN id SET DEFAULT nextval('sync_state.handler_error_log_id_seq'::regclass);
--
--   CREATE TABLE sync_state.raw_attributes (
--       chain_id character varying NOT NULL,
--       height bigint NOT NULL,
--       tx_index integer,
--       event_index integer NOT NULL,
--       key character varying NOT NULL,
--       value text,
--       composite_key character varying NOT NULL,
--       ingested_at timestamp with time zone DEFAULT now() NOT NULL
--   );
--
--   CREATE TABLE sync_state.raw_blocks (
--       chain_id character varying NOT NULL,
--       height bigint NOT NULL,
--       block_hash character varying NOT NULL,
--       block_time timestamp with time zone NOT NULL,
--       proposer character varying,
--       num_txs integer NOT NULL,
--       ingested_at timestamp with time zone DEFAULT now() NOT NULL
--   );
--
--   CREATE TABLE sync_state.raw_events (
--       chain_id character varying NOT NULL,
--       height bigint NOT NULL,
--       tx_index integer,
--       event_index integer NOT NULL,
--       event_type character varying NOT NULL,
--       ingested_at timestamp with time zone DEFAULT now() NOT NULL
--   );
--
--   CREATE TABLE sync_state.raw_tx_results (
--       chain_id character varying NOT NULL,
--       height bigint NOT NULL,
--       tx_index integer NOT NULL,
--       tx_hash character varying NOT NULL,
--       code integer NOT NULL,
--       gas_used bigint,
--       log text,
--       raw_json jsonb NOT NULL,
--       ingested_at timestamp with time zone DEFAULT now() NOT NULL
--   );
--
--   CREATE TABLE sync_state.sync_cursor (
--       chain_id character varying NOT NULL,
--       last_height bigint NOT NULL,
--       last_block_hash character varying,
--       last_block_time timestamp with time zone,
--       status text,
--       lag_blocks bigint,
--       tip_height bigint,
--       updated_at timestamp with time zone DEFAULT now() NOT NULL
--   );
--
--   CREATE TABLE sync_state.unknown_event_log (
--       chain_id character varying NOT NULL,
--       composite_key character varying NOT NULL,
--       count bigint DEFAULT 0 NOT NULL,
--       first_seen_height bigint NOT NULL,
--       last_seen_height bigint NOT NULL,
--       first_seen_at timestamp with time zone DEFAULT now() NOT NULL,
--       last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
--       last_payload jsonb
--   );
--
--   CREATE TABLE sync_state.verification_report (
--       run_id uuid NOT NULL,
--       scope text NOT NULL,
--       height bigint,
--       composite_key text,
--       expected jsonb,
--       actual jsonb,
--       status text NOT NULL,
--       created_at timestamp with time zone DEFAULT now() NOT NULL
--   );
--
--   -- Primary-key constraints
--   ALTER TABLE ONLY sync_state.block_log
--       ADD CONSTRAINT block_log_pkey PRIMARY KEY (chain_id, height);
--   ALTER TABLE ONLY sync_state.genesis_log
--       ADD CONSTRAINT genesis_log_pkey PRIMARY KEY (chain_id);
--   ALTER TABLE ONLY sync_state.handler_error_log
--       ADD CONSTRAINT handler_error_log_pkey PRIMARY KEY (id);
--   ALTER TABLE ONLY sync_state.raw_blocks
--       ADD CONSTRAINT raw_blocks_pkey PRIMARY KEY (chain_id, height);
--   ALTER TABLE ONLY sync_state.raw_tx_results
--       ADD CONSTRAINT raw_tx_results_pkey PRIMARY KEY (chain_id, height, tx_index);
--   ALTER TABLE ONLY sync_state.sync_cursor
--       ADD CONSTRAINT sync_cursor_pkey PRIMARY KEY (chain_id);
--   ALTER TABLE ONLY sync_state.unknown_event_log
--       ADD CONSTRAINT unknown_event_log_pkey PRIMARY KEY (chain_id, composite_key);
--
--   -- Secondary indexes
--   CREATE INDEX handler_error_log_severity_unresolved_idx
--       ON sync_state.handler_error_log USING btree (chain_id, severity)
--       WHERE (resolved_at IS NULL);
--   CREATE INDEX handler_error_log_unresolved_idx
--       ON sync_state.handler_error_log USING btree (chain_id, composite_key, created_at)
--       WHERE (resolved_at IS NULL);
--   CREATE INDEX raw_attributes_composite_key_idx
--       ON sync_state.raw_attributes USING btree (composite_key, height);
--   CREATE INDEX raw_attributes_height_idx
--       ON sync_state.raw_attributes USING btree (chain_id, height);
--   CREATE INDEX raw_events_height_idx
--       ON sync_state.raw_events USING btree (chain_id, height);
--   CREATE INDEX unknown_event_log_count_idx
--       ON sync_state.unknown_event_log USING btree (chain_id, count DESC);
