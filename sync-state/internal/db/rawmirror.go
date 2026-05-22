package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RawMirrorBlock is one row for sync_state.raw_blocks.
type RawMirrorBlock struct {
	ChainID    string
	Height     int64
	BlockHash  string
	BlockTime  time.Time
	Proposer   string
	NumTxs     int
}

// RawMirrorTxResult is one row for sync_state.raw_tx_results.
type RawMirrorTxResult struct {
	ChainID  string
	Height   int64
	TxIndex  int
	TxHash   string
	Code     int
	GasUsed  *int64
	Log      string
	RawJSON  json.RawMessage
}

// RawMirrorEvent is one row for sync_state.raw_events.
type RawMirrorEvent struct {
	ChainID    string
	Height     int64
	TxIndex    *int // nil for finalize_block events
	EventIndex int
	EventType  string
}

// RawMirrorAttribute is one row for sync_state.raw_attributes.
type RawMirrorAttribute struct {
	ChainID      string
	Height       int64
	TxIndex      *int
	EventIndex   int
	Key          string
	Value        string
	CompositeKey string
}

// WriteRawBlock inserts one row into sync_state.raw_blocks. UPSERT so
// re-ingestion is idempotent.
func WriteRawBlock(ctx context.Context, tx pgx.Tx, b RawMirrorBlock) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sync_state.raw_blocks
			(chain_id, height, block_hash, block_time, proposer, num_txs, ingested_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (chain_id, height) DO UPDATE
		   SET block_hash = EXCLUDED.block_hash,
		       block_time = EXCLUDED.block_time,
		       proposer   = EXCLUDED.proposer,
		       num_txs    = EXCLUDED.num_txs,
		       ingested_at = NOW()
	`, b.ChainID, b.Height, b.BlockHash, b.BlockTime, b.Proposer, b.NumTxs)
	return err
}

// WriteRawTxResults inserts tx_results in bulk via CopyFrom.
// Identifiers are compile-time only (no chain-derived strings).
func WriteRawTxResults(ctx context.Context, tx pgx.Tx, rows []RawMirrorTxResult) error {
	if len(rows) == 0 {
		return nil
	}
	// CopyFrom doesn't support ON CONFLICT, so we first delete any matching
	// rows for this (chain_id, height) range, then bulk-insert.
	heights := uniqueHeights(rows)
	if _, err := tx.Exec(ctx,
		`DELETE FROM sync_state.raw_tx_results WHERE chain_id = $1 AND height = ANY($2)`,
		rows[0].ChainID, heights,
	); err != nil {
		return fmt.Errorf("clear raw_tx_results: %w", err)
	}
	src := make([][]any, len(rows))
	for i, r := range rows {
		src[i] = []any{r.ChainID, r.Height, r.TxIndex, r.TxHash, r.Code, r.GasUsed, r.Log, r.RawJSON}
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"sync_state", "raw_tx_results"},
		[]string{"chain_id", "height", "tx_index", "tx_hash", "code", "gas_used", "log", "raw_json"},
		pgx.CopyFromRows(src),
	)
	return err
}

// WriteRawEvents bulk inserts events. Idempotency: caller is responsible for
// ensuring this height isn't already mirrored (typically because the per-block
// tx pre-deletes any previous run for the same height).
func WriteRawEvents(ctx context.Context, tx pgx.Tx, rows []RawMirrorEvent) error {
	if len(rows) == 0 {
		return nil
	}
	heights := uniqueHeightsEv(rows)
	if _, err := tx.Exec(ctx,
		`DELETE FROM sync_state.raw_events WHERE chain_id = $1 AND height = ANY($2)`,
		rows[0].ChainID, heights,
	); err != nil {
		return fmt.Errorf("clear raw_events: %w", err)
	}
	src := make([][]any, len(rows))
	for i, r := range rows {
		src[i] = []any{r.ChainID, r.Height, r.TxIndex, r.EventIndex, r.EventType}
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"sync_state", "raw_events"},
		[]string{"chain_id", "height", "tx_index", "event_index", "event_type"},
		pgx.CopyFromRows(src),
	)
	return err
}

// WriteRawAttributes bulk inserts attributes. Same idempotency notes as
// WriteRawEvents.
func WriteRawAttributes(ctx context.Context, tx pgx.Tx, rows []RawMirrorAttribute) error {
	if len(rows) == 0 {
		return nil
	}
	heights := uniqueHeightsAttr(rows)
	if _, err := tx.Exec(ctx,
		`DELETE FROM sync_state.raw_attributes WHERE chain_id = $1 AND height = ANY($2)`,
		rows[0].ChainID, heights,
	); err != nil {
		return fmt.Errorf("clear raw_attributes: %w", err)
	}
	src := make([][]any, len(rows))
	for i, r := range rows {
		src[i] = []any{r.ChainID, r.Height, r.TxIndex, r.EventIndex, r.Key, r.Value, r.CompositeKey}
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"sync_state", "raw_attributes"},
		[]string{"chain_id", "height", "tx_index", "event_index", "key", "value", "composite_key"},
		pgx.CopyFromRows(src),
	)
	return err
}

func uniqueHeights(rows []RawMirrorTxResult) []int64 {
	seen := map[int64]struct{}{}
	out := make([]int64, 0, 1)
	for _, r := range rows {
		if _, ok := seen[r.Height]; ok {
			continue
		}
		seen[r.Height] = struct{}{}
		out = append(out, r.Height)
	}
	return out
}

func uniqueHeightsEv(rows []RawMirrorEvent) []int64 {
	seen := map[int64]struct{}{}
	out := make([]int64, 0, 1)
	for _, r := range rows {
		if _, ok := seen[r.Height]; ok {
			continue
		}
		seen[r.Height] = struct{}{}
		out = append(out, r.Height)
	}
	return out
}

func uniqueHeightsAttr(rows []RawMirrorAttribute) []int64 {
	seen := map[int64]struct{}{}
	out := make([]int64, 0, 1)
	for _, r := range rows {
		if _, ok := seen[r.Height]; ok {
			continue
		}
		seen[r.Height] = struct{}{}
		out = append(out, r.Height)
	}
	return out
}
