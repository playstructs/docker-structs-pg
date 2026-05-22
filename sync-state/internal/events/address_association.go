package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// addressAssociationHandler ports cache.handle_event_address_association
// (cache-trigger-add-queue-20260223-add-player-address.sql:5-52, the
// player-exists-guarded successor to the bigly-refactor body).
//
// Composes player_id as "1-" + playerIndex, the same way the SQL does.
// SILENTLY skips if structs.player row for that id doesn't exist — this
// matches the 20260223 guard.
//
// Both this handler and addressHandler UPSERT structs.player_address
// keyed on (address). When both events fire for the same address in a
// block, chain order determines the final state of status/player_id.
type addressAssociationHandler struct{}

func (addressAssociationHandler) CompositeKey() string {
	return "structs.structs.EventAddressAssociation.addressAssociation"
}

const addressAssociationCheckSQL = `SELECT EXISTS (SELECT 1 FROM structs.player WHERE id = $1)`

const addressAssociationUpsertSQL = `
INSERT INTO structs.player_address (
    address, player_id, guild_id, status, created_at, updated_at
) VALUES (
    $1::varchar,
    $2::varchar,
    (SELECT guild_id FROM structs.player WHERE id = $2::varchar),
    $3,
    NOW(),
    NOW()
)
ON CONFLICT (address) DO UPDATE
   SET status     = EXCLUDED.status,
       player_id  = EXCLUDED.player_id,
       updated_at = EXCLUDED.updated_at
 WHERE structs.player_address.status    IS DISTINCT FROM EXCLUDED.status
    OR structs.player_address.player_id IS DISTINCT FROM EXCLUDED.player_id`

func (addressAssociationHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.AddressAssociation](raw)
	if err != nil {
		return err
	}
	if p.Address == "" {
		return fmt.Errorf("address_association: empty address")
	}
	playerID := "1-" + strconv.FormatInt(p.PlayerIndex.Int64(), 10)

	// 20260223 guard: skip silently if the player doesn't exist yet.
	var exists bool
	if err := tx.QueryRow(ctx, addressAssociationCheckSQL, playerID).Scan(&exists); err != nil {
		return fmt.Errorf("address_association player check (%s): %w", playerID, err)
	}
	if !exists {
		return nil
	}

	if _, err := tx.Exec(ctx, addressAssociationUpsertSQL,
		p.Address,
		playerID,
		payload.NullableText(p.RegistrationStatus),
	); err != nil {
		return fmt.Errorf("address_association upsert addr=%s pid=%s: %w", p.Address, playerID, err)
	}
	return nil
}
