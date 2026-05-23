package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// GenesisLogRow mirrors sync_state.genesis_log. Pointer-free since every
// column is NOT NULL.
type GenesisLogRow struct {
	ChainID         string
	AppliedAt       time.Time
	Source          string
	GenesisTime     time.Time
	SHA256          string
	RowsPerSection  map[string]int64
	TotalRows       int64
}

// ReadGenesisLog returns the row for chainID, or (nil, nil) when no row
// exists yet (genesis not applied). Any other error is returned verbatim.
// Tolerates the genesis_log table not existing yet for callers that run
// before bootstrap (e.g. ingest's pre-bootstrap auto-load decision) —
// returns (nil, nil) in that case too.
func ReadGenesisLog(ctx context.Context, q Querier, chainID string) (*GenesisLogRow, error) {
	var (
		row     GenesisLogRow
		rpsJSON []byte
	)
	err := q.QueryRow(ctx, `
		SELECT chain_id, applied_at, source, genesis_time, sha256, rows_per_section, total_rows
		  FROM sync_state.genesis_log
		 WHERE chain_id = $1
	`, chainID).Scan(
		&row.ChainID, &row.AppliedAt, &row.Source, &row.GenesisTime,
		&row.SHA256, &rpsJSON, &row.TotalRows,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(rpsJSON) > 0 {
		row.RowsPerSection = make(map[string]int64)
		if err := json.Unmarshal(rpsJSON, &row.RowsPerSection); err != nil {
			return nil, fmt.Errorf("genesis_log: decode rows_per_section: %w", err)
		}
	}
	return &row, nil
}

// WriteGenesisLog inserts or replaces the genesis_log row for chainID.
// The Apply path calls this inside the same tx that does the inserts so
// either both land or neither does — operators can re-run init-genesis
// safely without worrying about half-applied state.
func WriteGenesisLog(ctx context.Context, tx pgx.Tx, row GenesisLogRow) error {
	rps, err := json.Marshal(row.RowsPerSection)
	if err != nil {
		return fmt.Errorf("genesis_log: encode rows_per_section: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO sync_state.genesis_log
			(chain_id, applied_at, source, genesis_time, sha256, rows_per_section, total_rows)
		VALUES ($1, NOW(), $2, $3, $4, $5, $6)
		ON CONFLICT (chain_id) DO UPDATE
		   SET applied_at = EXCLUDED.applied_at,
			   source = EXCLUDED.source,
			   genesis_time = EXCLUDED.genesis_time,
			   sha256 = EXCLUDED.sha256,
			   rows_per_section = EXCLUDED.rows_per_section,
			   total_rows = EXCLUDED.total_rows
	`, row.ChainID, row.Source, row.GenesisTime, row.SHA256, rps, row.TotalRows)
	return err
}

// DeleteGenesisLedgerRows removes every structs.ledger row tagged
// action='genesis'. Used by Apply for replay safety — re-running
// init-genesis truncates the previous load before re-inserting.
//
// structs.ledger has no chain_id column (single-chain DB by design, see
// `indexer-insert-genesis.sh` in docker-structsd which does the same),
// so this is a global wipe of the genesis tag; safe because the
// genesis_log row is the single source of truth for "is genesis applied?".
func DeleteGenesisLedgerRows(ctx context.Context, tx pgx.Tx) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM structs.ledger WHERE action = 'genesis'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// isUndefinedTable returns true when err is the pg "table doesn't
// exist" SQLSTATE (42P01). Used by ReadGenesisLog so the auto-load
// decision in ingest works on a fresh DB where bootstrap hasn't created
// the table yet (we want "no row" semantics, not a hard error).
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return true
	}
	return false
}
