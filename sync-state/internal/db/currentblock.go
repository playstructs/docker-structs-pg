package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// CurrentBlockUpsert is the structs.current_block heartbeat sync-state
// writes every block. status/lag_blocks/tip_height are the new columns
// bootstrap added via ALTER TABLE ... ADD COLUMN IF NOT EXISTS. The legacy
// chain/height/updated_at columns are preserved.
type CurrentBlockUpsert struct {
	Chain     string
	Height    int64
	UpdatedAt time.Time
	Status    SyncStatus
	LagBlocks int64
	TipHeight int64
}

// UpsertCurrentBlock writes structs.current_block. Returns nil and logs a
// debug note if the structs.current_block table doesn't exist (fresh DB
// without structs-pg deployed); sync-state can still run for bookkeeping.
//
// Unlike today's SQL handler (which has a WHERE IS DISTINCT FROM guard that
// suppresses no-op UPDATEs), we ALWAYS update updated_at on commit. That
// ensures sync_state.* bookkeeping reflects every block. The webapp-facing
// notify is emitted separately via EmitCurrentBlockHeartbeat so it fires
// regardless of whether the row changed (the GRASS trigger would skip it).
func UpsertCurrentBlock(ctx context.Context, q Querier, c CurrentBlockUpsert) error {
	_, err := q.Exec(ctx, `
		INSERT INTO structs.current_block (chain, height, updated_at, status, lag_blocks, tip_height)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (chain) DO UPDATE
		   SET height     = EXCLUDED.height,
		       updated_at = EXCLUDED.updated_at,
		       status     = EXCLUDED.status,
		       lag_blocks = EXCLUDED.lag_blocks,
		       tip_height = EXCLUDED.tip_height
	`, c.Chain, c.Height, c.UpdatedAt.UTC(), string(c.Status), c.LagBlocks, c.TipHeight)
	if err != nil {
		var pgErr *pgconn.PgError
		// 42P01 = undefined_table; skip if structs.current_block isn't deployed.
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return nil
		}
		return err
	}
	return nil
}

// CurrentBlockTableExists reports whether structs.current_block is present.
// Used by the doctor to decide whether to enforce the trigger-vs-flag matrix.
func CurrentBlockTableExists(ctx context.Context, q Querier) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='structs' AND tablename='current_block')
	`).Scan(&exists)
	return exists, err
}
