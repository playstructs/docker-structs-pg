package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bootstrap creates the sync_state schema, its bookkeeping tables, the
// optional raw-mirror tables, and the column additions on structs.* that
// downstream phases will populate. Every statement is idempotent so this can
// run on every startup (including against a structs-pg that already has every
// object).
//
// Schema introduced:
//   - sync_state.sync_cursor          (one row per chain_id, the resume point)
//   - sync_state.handler_error_log    (per-event handler failures)
//   - sync_state.block_log            (per-block audit row, FK-free)
//   - sync_state.verification_report  (verify subcommand output)
//   - sync_state.raw_blocks           (optional raw mirror, same shape as cache.blocks)
//   - sync_state.raw_tx_results       (optional raw mirror)
//   - sync_state.raw_events           (optional raw mirror)
//   - sync_state.raw_attributes       (optional raw mirror; high-churn)
//
// Soft column additions on existing structs.* tables:
//   - structs.current_block: status TEXT, lag_blocks BIGINT, tip_height BIGINT
//   - structs.planet_activity: block_height BIGINT
//   - structs.stat_<x>: block_height BIGINT  (for each known stat table)
//
// All ALTERs are nullable + IF NOT EXISTS so existing SQL handlers continue
// to work; sync-state populates the new columns.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range bootstrapStatements {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap: %w\nSQL: %s", err, firstLine(stmt))
		}
	}
	return nil
}

// BootstrapTx variant that runs inside an existing transaction (useful for
// tests).
func BootstrapTx(ctx context.Context, tx pgx.Tx) error {
	for _, stmt := range bootstrapStatements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap: %w\nSQL: %s", err, firstLine(stmt))
		}
	}
	return nil
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

// statKnownTables is the canonical list of structs.stat_* tables that take
// a block_height column. Mirrors the deploy/table-stat.sql layout exactly:
// stat_connection_count, NOT stat_connection_load — the old bootstrap had
// the wrong name and the grid handler always wrote to stat_connection_count
// (matches cache.handle_event_grid sub_idx=7). Phase A5 fix.
var statKnownTables = []string{
	"stat_ore",
	"stat_fuel",
	"stat_capacity",
	"stat_load",
	"stat_structs_load",
	"stat_power",
	"stat_connection_capacity",
	"stat_connection_count",
	"stat_struct_health",
	"stat_struct_status",
}

// StatTables returns the allowlist of structs.stat_* table names. Used by
// Phase 4 handlers to ensure pgx.Identifier is only ever constructed from
// compile-time-known strings.
func StatTables() []string {
	out := make([]string, len(statKnownTables))
	copy(out, statKnownTables)
	return out
}

// bootstrapStatements is the full idempotent setup. Order matters for
// pg_dump-like readability; PG itself doesn't require an order beyond
// schema-first.
var bootstrapStatements = []string{
	// --- sync_state schema --------------------------------------------------
	`CREATE SCHEMA IF NOT EXISTS sync_state`,

	`CREATE TABLE IF NOT EXISTS sync_state.sync_cursor (
		chain_id          VARCHAR PRIMARY KEY,
		last_height       BIGINT NOT NULL,
		last_block_hash   VARCHAR,
		last_block_time   TIMESTAMPTZ,
		status            TEXT,
		lag_blocks        BIGINT,
		tip_height        BIGINT,
		updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,

	`CREATE TABLE IF NOT EXISTS sync_state.handler_error_log (
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
	)`,

	// severity column added later via ALTER for existing deployments;
	// the CREATE above already includes it for fresh databases.
	`ALTER TABLE sync_state.handler_error_log
		ADD COLUMN IF NOT EXISTS severity TEXT NOT NULL DEFAULT 'error'`,

	`CREATE INDEX IF NOT EXISTS handler_error_log_unresolved_idx
		ON sync_state.handler_error_log (chain_id, composite_key, created_at)
		WHERE resolved_at IS NULL`,

	// Severity-aware index for the startup banner + verify subcommand
	// (both want fast SELECT severity, COUNT(*) ... WHERE resolved IS NULL).
	`CREATE INDEX IF NOT EXISTS handler_error_log_severity_unresolved_idx
		ON sync_state.handler_error_log (chain_id, severity)
		WHERE resolved_at IS NULL`,

	`CREATE TABLE IF NOT EXISTS sync_state.block_log (
		chain_id            VARCHAR NOT NULL,
		height              BIGINT NOT NULL,
		block_hash          VARCHAR NOT NULL,
		block_time          TIMESTAMPTZ NOT NULL,
		num_txs             INT NOT NULL,
		num_events          INT NOT NULL,
		num_handler_errors  INT NOT NULL DEFAULT 0,
		ingested_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (chain_id, height)
	)`,

	`CREATE TABLE IF NOT EXISTS sync_state.verification_report (
		run_id        UUID NOT NULL,
		scope         TEXT NOT NULL,
		height        BIGINT,
		composite_key TEXT,
		expected      JSONB,
		actual        JSONB,
		status        TEXT NOT NULL,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,

	// --- raw mirror tables (always created; written only when SYNC_STATE_MIRROR_RAW=true) ---
	`CREATE TABLE IF NOT EXISTS sync_state.raw_blocks (
		chain_id     VARCHAR NOT NULL,
		height       BIGINT NOT NULL,
		block_hash   VARCHAR NOT NULL,
		block_time   TIMESTAMPTZ NOT NULL,
		proposer     VARCHAR,
		num_txs      INT NOT NULL,
		ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (chain_id, height)
	)`,

	`CREATE TABLE IF NOT EXISTS sync_state.raw_tx_results (
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
	)`,

	`CREATE TABLE IF NOT EXISTS sync_state.raw_events (
		chain_id     VARCHAR NOT NULL,
		height       BIGINT NOT NULL,
		tx_index     INT,
		event_index  INT NOT NULL,
		event_type   VARCHAR NOT NULL,
		ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,

	`CREATE INDEX IF NOT EXISTS raw_events_height_idx
		ON sync_state.raw_events (chain_id, height)`,

	`CREATE TABLE IF NOT EXISTS sync_state.raw_attributes (
		chain_id      VARCHAR NOT NULL,
		height        BIGINT NOT NULL,
		tx_index      INT,
		event_index   INT NOT NULL,
		key           VARCHAR NOT NULL,
		value         TEXT,
		composite_key VARCHAR NOT NULL,
		ingested_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,

	`CREATE INDEX IF NOT EXISTS raw_attributes_height_idx
		ON sync_state.raw_attributes (chain_id, height)`,

	`CREATE INDEX IF NOT EXISTS raw_attributes_composite_key_idx
		ON sync_state.raw_attributes (composite_key, height)`,

	// --- unknown_event_log ---------------------------------------------------
	//
	// One row per distinct composite_key (event_type.attribute_key) that the
	// router did NOT have a registered handler for. Acts as a coverage
	// dashboard for "are we processing every attribute the chain emits?" —
	// operators can query this table at any time to see what's been skipped
	// and decide whether a new handler is needed.
	//
	// count / first_seen_height / last_seen_height accumulate monotonically
	// across ingest runs (UPSERT with GREATEST / LEAST). last_payload holds
	// the most-recent raw attribute value as a JSON literal so operators can
	// pivot directly into sync_state.raw_attributes for full samples.
	`CREATE TABLE IF NOT EXISTS sync_state.unknown_event_log (
		chain_id           VARCHAR NOT NULL,
		composite_key      VARCHAR NOT NULL,
		count              BIGINT  NOT NULL DEFAULT 0,
		first_seen_height  BIGINT  NOT NULL,
		last_seen_height   BIGINT  NOT NULL,
		first_seen_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_payload       JSONB,
		PRIMARY KEY (chain_id, composite_key)
	)`,

	`CREATE INDEX IF NOT EXISTS unknown_event_log_count_idx
		ON sync_state.unknown_event_log (chain_id, count DESC)`,

	// --- genesis_log ---------------------------------------------------------
	//
	// One row per (chain_id) that has had its genesis JSON applied to
	// structs.ledger (action='genesis'). Acts as the idempotency guard for
	// `sync-state init-genesis`: ingest auto-applies on the first start
	// from height 1 when this row is missing, and the verify check FAILs
	// loudly when cursor>0 but genesis was never loaded (which is what
	// produces the false-alarm "negative balance" rows seen during the
	// pre-genesis-import test runs — staking debits with no matching
	// genesis credits).
	//
	// source is "rpc:<url>" or "file:<path>" so the post-mortem trail is
	// obvious. sha256 is over the raw genesis JSON bytes (pre-parse) so a
	// reapply against a tampered genesis is detectable. rows_per_section
	// is a small JSONB ({"bank":138,"delegations":40,...}) the verify
	// runner can surface without re-parsing genesis.
	`CREATE TABLE IF NOT EXISTS sync_state.genesis_log (
		chain_id           VARCHAR PRIMARY KEY,
		applied_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		source             TEXT NOT NULL,
		genesis_time       TIMESTAMPTZ NOT NULL,
		sha256             VARCHAR(64) NOT NULL,
		rows_per_section   JSONB NOT NULL,
		total_rows         BIGINT NOT NULL
	)`,

	// The pre-cutover bootstrap also added structs.current_block.{status,
	// lag_blocks,tip_height}, structs.planet_activity.block_height, and
	// structs.stat_*.block_height via ALTER ... ADD COLUMN IF NOT EXISTS
	// DO blocks. Those columns now ship as part of canonical structs-pg
	// (see sync-state/sql/retire-cache.sql step 1). The runtime ALTERs
	// are gone; the doctor probe `canonical schema` (see
	// internal/doctor/probe_canonical.go) FATALs if any column is missing
	// so we refuse to start against a DB that hasn't applied
	// retire-cache.sql yet.
}
