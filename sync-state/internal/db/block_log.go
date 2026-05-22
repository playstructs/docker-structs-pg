package db

import (
	"context"
	"time"
)

// BlockLogEntry is one row of sync_state.block_log.
type BlockLogEntry struct {
	ChainID          string
	Height           int64
	BlockHash        string
	BlockTime        time.Time
	NumTxs           int
	NumEvents        int
	NumHandlerErrors int
}

// WriteBlockLog inserts (or updates, if re-ingesting) one row.
func WriteBlockLog(ctx context.Context, q Querier, e BlockLogEntry) error {
	_, err := q.Exec(ctx, `
		INSERT INTO sync_state.block_log
			(chain_id, height, block_hash, block_time, num_txs, num_events, num_handler_errors, ingested_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (chain_id, height) DO UPDATE
		   SET block_hash         = EXCLUDED.block_hash,
		       block_time         = EXCLUDED.block_time,
		       num_txs            = EXCLUDED.num_txs,
		       num_events         = EXCLUDED.num_events,
		       num_handler_errors = EXCLUDED.num_handler_errors,
		       ingested_at        = NOW()
	`, e.ChainID, e.Height, e.BlockHash, e.BlockTime, e.NumTxs, e.NumEvents, e.NumHandlerErrors)
	return err
}

// LookupBlockHash returns the block_hash for a previously-ingested height, or
// "" if not present. Used for reorg detection on startup.
func LookupBlockHash(ctx context.Context, q Querier, chainID string, height int64) (string, error) {
	var hash string
	err := q.QueryRow(ctx,
		`SELECT block_hash FROM sync_state.block_log WHERE chain_id = $1 AND height = $2`,
		chainID, height,
	).Scan(&hash)
	if err != nil {
		return "", err
	}
	return hash, nil
}
