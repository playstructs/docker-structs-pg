package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DeletePlanetActivityAfter removes planet_activity rows above height and
// the matching planet_activity_player side rows in the same transaction.
// There is no FK between the two hypertables; forgetting the side delete
// leaves orphans that the nightly reconciler logs as state='orphan'.
//
// Re-inserted rows are attributed by the trigger with ownership as it is
// at re-insert time, not as-of the original event.
func DeletePlanetActivityAfter(ctx context.Context, tx pgx.Tx, height int64) error {
	if _, err := tx.Exec(ctx, `
DELETE FROM structs.planet_activity_player
 WHERE block_height > $1`, height); err != nil {
		return fmt.Errorf("delete planet_activity_player after %d: %w", height, err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM structs.planet_activity
 WHERE block_height > $1`, height); err != nil {
		return fmt.Errorf("delete planet_activity after %d: %w", height, err)
	}
	return nil
}
