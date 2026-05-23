package events

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// upsertPlayerObject writes the structs.player_object sidecar shared by 9
// of the 12 Phase 2 entity handlers (every owner-bearing entity plus
// player's self-mapping). Mirrors the SQL pattern:
//
//	INSERT INTO structs.player_object(object_id, player_id) VALUES ($1,$2)
//	ON CONFLICT (object_id) DO UPDATE SET player_id = EXCLUDED.player_id
//	WHERE structs.player_object.player_id IS DISTINCT FROM EXCLUDED.player_id;
//
// Returns nil if playerID is empty (no owner == no sidecar row, matching
// SQL behavior where v.owner is NULL).
func upsertPlayerObject(ctx context.Context, tx pgx.Tx, objectID, playerID string) error {
	if objectID == "" {
		return fmt.Errorf("player_object: empty object_id")
	}
	if playerID == "" {
		return nil
	}
	const sql = `
INSERT INTO structs.player_object (object_id, player_id)
VALUES ($1, $2)
ON CONFLICT (object_id) DO UPDATE
   SET player_id = EXCLUDED.player_id
 WHERE structs.player_object.player_id IS DISTINCT FROM EXCLUDED.player_id`
	if _, err := tx.Exec(ctx, sql, objectID, playerID); err != nil {
		return fmt.Errorf("player_object upsert (%s -> %s): %w", objectID, playerID, err)
	}
	return nil
}
