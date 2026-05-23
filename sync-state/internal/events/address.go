package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// addressHandler ports cache.handle_event_address
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1533-1574).
//
// UPSERT structs.player_address. On INSERT: hardcoded status='approved'
// and guild_id is looked up from structs.player. On UPDATE: only status
// and player_id are refreshed — guild_id is INTENTIONALLY left alone
// (matches the SQL handler). Guild propagation is owned by playerHandler
// (see playerAddressGuildPropagateSQL in player.go) — when the player's
// guild changes, that handler explicitly updates every player_address
// row for the player. The structs.PLAYER_ADDRESS_CASCADE trigger
// (trigger-player-address-cascade.sql) does the same propagation as a
// defense-in-depth fallback for any direct UPDATE to structs.player.
// The retired cache.UDPATE_ADDRESS_GUILD trigger is dropped by Phase B.
//
// Ordering: requires structs.player to exist for the INSERT's guild_id
// lookup. If the player isn't there, guild_id is NULL on INSERT and
// playerHandler propagates the new guild_id when the player event
// arrives.
//
// PG triggers on structs.player_address that we deliberately leave in
// place (the doctor does not flag them; the Phase A6 audit confirmed
// none read from cache.*):
//   - PLAYER_ADDRESS_NOTIFY (grass SSE — reads NEW only)
//   - PLAYER_ADDRESS_PENDING_MERGE (clears player_address_pending on insert)
//   - PLAYER_ADDRESS_CASCADE (guild propagation fallback, see above)
type addressHandler struct{}

func (addressHandler) CompositeKey() string {
	return "structs.structs.EventAddress.address"
}

// Note: $2 appears in both the player_id column slot and the sub-SELECT
// predicate; pgx cannot deduce a single type so we cast it explicitly
// (matches structs.player.id's varchar type).
const addressUpsertSQL = `
INSERT INTO structs.player_address (
    address, player_id, guild_id, status, created_at, updated_at
) VALUES (
    $1::varchar,
    $2::varchar,
    (SELECT guild_id FROM structs.player WHERE id = $2::varchar),
    'approved',
    NOW(),
    NOW()
)
ON CONFLICT (address) DO UPDATE
   SET status     = EXCLUDED.status,
       player_id  = EXCLUDED.player_id,
       updated_at = EXCLUDED.updated_at
 WHERE structs.player_address.status    IS DISTINCT FROM EXCLUDED.status
    OR structs.player_address.player_id IS DISTINCT FROM EXCLUDED.player_id`

func (addressHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Address](raw)
	if err != nil {
		return err
	}
	if p.Address == "" {
		return fmt.Errorf("address: empty address")
	}
	if _, err := tx.Exec(ctx, addressUpsertSQL, p.Address, payload.NullableText(p.PlayerID)); err != nil {
		return fmt.Errorf("address upsert addr=%s: %w", p.Address, err)
	}
	return nil
}
