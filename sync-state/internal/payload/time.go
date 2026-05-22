package payload

import "time"

// Time matches structs.structs.EventTime.eventTimeDetail.
// SQL handler: cache.handle_event_time
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2127-2151)
//
// blockHeight comes through as a JSON string for int64 precision.
// blockTime is RFC3339.
//
// The Go sync-state already upserts structs.current_block per block via
// db.UpsertCurrentBlock with richer columns (status / tip_height /
// lag_blocks). This payload is here so the timeHandler can verify the
// chain-emitted heartbeat matches what sync-state already wrote, but the
// handler itself is intentionally a no-op (see internal/events/time.go).
type Time struct {
	BlockHeight JSONInt   `json:"blockHeight"`
	BlockTime   time.Time `json:"blockTime"`
}
