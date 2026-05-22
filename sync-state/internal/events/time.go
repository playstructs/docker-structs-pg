package events

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// timeHandler is intentionally a NO-OP.
//
// The SQL counterpart, cache.handle_event_time
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2127-2151), upserts
// structs.current_block with the payload's (blockHeight, blockTime). In
// Go we own that write via db.UpsertCurrentBlock, which fires every block
// from sync.applyBlock with a *richer* payload (status / tip_height /
// lag_blocks). Running both writers would either race or clobber the
// richer columns, so we register this no-op handler to:
//
//   1. Acknowledge the EventTime so it doesn't get counted as an unknown
//      composite_key (and possibly fatal under -strict-unknown-events).
//   2. Document in code that current_block ownership is on the sync
//      layer, not on a per-event handler.
//
// The payload struct (payload.Time) is still defined so callers/tests
// can decode and assert the chain heartbeat matches what we wrote.
type timeHandler struct{}

func (timeHandler) CompositeKey() string {
	return "structs.structs.EventTime.eventTimeDetail"
}

func (timeHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	return nil
}
