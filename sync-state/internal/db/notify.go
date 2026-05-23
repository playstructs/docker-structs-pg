package db

import (
	"context"
	"encoding/json"
	"time"
)

// CurrentBlockHeartbeat is the JSON payload the original
// structs.CURRENT_BLOCK_NOTIFY trigger emits on AFTER INSERT/UPDATE of
// structs.current_block. We emit it directly from sync-state so we don't
// depend on the trigger (which is suppressed when the UPSERT is a no-op
// because of the WHERE IS DISTINCT FROM guard the SQL handler uses today).
//
// Keep the field set and JSON keys IDENTICAL to the trigger's output so the
// webapp's existing LISTEN parser keeps working.
type CurrentBlockHeartbeat struct {
	Subject   string    `json:"subject"`   // "consensus"
	Category  string    `json:"category"`  // "block"
	Height    int64     `json:"height"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EmitCurrentBlockHeartbeat emits the consensus/block notify on channel
// 'grass'. Should be called inside the per-block transaction so it commits or
// rolls back with the rest of the block.
func EmitCurrentBlockHeartbeat(ctx context.Context, q Querier, height int64, blockTime time.Time) error {
	payload := CurrentBlockHeartbeat{
		Subject:   "consensus",
		Category:  "block",
		Height:    height,
		UpdatedAt: blockTime.UTC(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `SELECT pg_notify('grass', $1)`, string(body))
	return err
}
