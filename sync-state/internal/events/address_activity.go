package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// addressActivityHandler ports cache.handle_event_address_activity
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1493-1531).
//
// UPSERT structs.player_address_activity keyed on address. player_id is
// resolved at INSERT time via a sub-SELECT on structs.player_address —
// if the address has no bound player yet (event arrived before address
// or address_association), player_id remains NULL. On UPDATE we only
// refresh block_height/block_time (player_id stays as-is until the next
// INSERT path runs after a row delete).
//
// Ordering: ideally `address` / `addressAssociation` before this. Within
// a block we honor the chain's emit order, which gets this right.
type addressActivityHandler struct{}

func (addressActivityHandler) CompositeKey() string {
	return "structs.structs.EventAddressActivity.addressActivity"
}

// Note: $1 is used both as the inserted address and inside the player_id
// sub-SELECT; pgx needs an explicit cast to deduce a single type.
const addressActivityUpsertSQL = `
INSERT INTO structs.player_address_activity (
    address, player_id, block_height, block_time
) VALUES (
    $1::varchar,
    (SELECT player_id FROM structs.player_address WHERE address = $1::varchar),
    $2,
    $3
)
ON CONFLICT (address) DO UPDATE
   SET block_height = EXCLUDED.block_height,
       block_time   = EXCLUDED.block_time
 WHERE structs.player_address_activity.block_height IS DISTINCT FROM EXCLUDED.block_height
    OR structs.player_address_activity.block_time   IS DISTINCT FROM EXCLUDED.block_time`

func (addressActivityHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.AddressActivity](raw)
	if err != nil {
		return err
	}
	if p.Address == "" {
		return fmt.Errorf("address_activity: empty address")
	}
	if _, err := tx.Exec(ctx, addressActivityUpsertSQL,
		p.Address,
		p.BlockHeight.Int64(),
		p.BlockTime,
	); err != nil {
		return fmt.Errorf("address_activity upsert addr=%s: %w", p.Address, err)
	}
	return nil
}
