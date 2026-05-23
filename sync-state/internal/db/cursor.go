package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SyncStatus is the human-facing label sync-state writes into both
// sync_state.sync_cursor.status and structs.current_block.status.
//
// Computed on every commit and every tip-poll:
//   - catching_up: tip - height > 100
//   - syncing    : 1 < tip - height <= 100
//   - current    : tip - height <= 1
//   - stalled    : last commit older than several poll intervals AND lag > 0
//   - replaying  : set explicitly by the replay subcommand
type SyncStatus string

const (
	StatusCatchingUp SyncStatus = "catching_up"
	StatusSyncing    SyncStatus = "syncing"
	StatusCurrent    SyncStatus = "current"
	StatusStalled    SyncStatus = "stalled"
	StatusReplaying  SyncStatus = "replaying"
)

// ComputeStatus picks the right label from current lag.
func ComputeStatus(height, tip int64) SyncStatus {
	lag := tip - height
	switch {
	case lag <= 1:
		return StatusCurrent
	case lag <= 100:
		return StatusSyncing
	default:
		return StatusCatchingUp
	}
}

// Cursor mirrors the sync_state.sync_cursor row in memory.
type Cursor struct {
	ChainID       string
	LastHeight    int64
	LastBlockHash string
	LastBlockTime time.Time
	Status        SyncStatus
	LagBlocks     int64
	TipHeight     int64
	UpdatedAt     time.Time
}

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn, and pgx.Tx. Lets the
// cursor / block_log / error_log writers be called from both transactional
// and non-transactional contexts.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ReadCursor returns the cursor for chainID, or a zero-valued Cursor with
// LastHeight=0 if none exists yet.
func ReadCursor(ctx context.Context, q Querier, chainID string) (Cursor, error) {
	var c Cursor
	c.ChainID = chainID
	var (
		hashPtr  *string
		timePtr  *time.Time
		stat     *string
		lag, tip *int64
	)
	err := q.QueryRow(ctx, `
		SELECT last_height, last_block_hash, last_block_time, status, lag_blocks, tip_height, updated_at
		  FROM sync_state.sync_cursor
		 WHERE chain_id = $1
	`, chainID).Scan(&c.LastHeight, &hashPtr, &timePtr, &stat, &lag, &tip, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, nil
		}
		return c, fmt.Errorf("read sync_cursor: %w", err)
	}
	if hashPtr != nil {
		c.LastBlockHash = *hashPtr
	}
	if timePtr != nil {
		c.LastBlockTime = *timePtr
	}
	if stat != nil {
		c.Status = SyncStatus(*stat)
	}
	if lag != nil {
		c.LagBlocks = *lag
	}
	if tip != nil {
		c.TipHeight = *tip
	}
	return c, nil
}

// UpsertCursor writes the cursor row. Caller is responsible for being inside
// the per-block transaction (so the cursor advances atomically with the
// block's writes).
func UpsertCursor(ctx context.Context, q Querier, c Cursor) error {
	_, err := q.Exec(ctx, `
		INSERT INTO sync_state.sync_cursor
			(chain_id, last_height, last_block_hash, last_block_time, status, lag_blocks, tip_height, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (chain_id) DO UPDATE
		   SET last_height     = EXCLUDED.last_height,
		       last_block_hash = EXCLUDED.last_block_hash,
		       last_block_time = EXCLUDED.last_block_time,
		       status          = EXCLUDED.status,
		       lag_blocks      = EXCLUDED.lag_blocks,
		       tip_height      = EXCLUDED.tip_height,
		       updated_at      = NOW()
	`, c.ChainID, c.LastHeight, c.LastBlockHash, c.LastBlockTime, string(c.Status), c.LagBlocks, c.TipHeight)
	return err
}

// SetStatusOnly updates only the status / lag / tip columns. Used from the
// tip-poll loop when there's no block being committed.
func SetStatusOnly(ctx context.Context, q Querier, chainID string, status SyncStatus, lag, tip int64) error {
	_, err := q.Exec(ctx, `
		UPDATE sync_state.sync_cursor
		   SET status     = $2,
		       lag_blocks = $3,
		       tip_height = $4,
		       updated_at = NOW()
		 WHERE chain_id = $1
	`, chainID, string(status), lag, tip)
	return err
}
