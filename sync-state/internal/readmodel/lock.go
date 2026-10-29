package readmodel

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Lock serializes canonical writes and projection maintenance. Take it before
// reading a maintenance boundary or mutating canonical state, including bulk
// windows that defer projections. SQL maintenance uses the same existing work
// lock; acquiring only at Recompute would leave those windows unprotected.
func Lock(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('structs.api_work_refresh'))`); err != nil {
		return fmt.Errorf("lock projection maintenance: %w", err)
	}
	return nil
}

// Invalidate withholds readiness when an out-of-band canonical repair changes
// projection inputs. Startup will rebuild before streaming; scheduled jobs must
// not stamp the older projections as a complete snapshot in the meantime.
func Invalidate(ctx context.Context, tx pgx.Tx) error {
	// Keep the inventory authority across repairs/restarts. Dropping its
	// marker could re-enable legacy SQL comparisons or unsafe schema rollback.
	_, err := tx.Exec(ctx, `DELETE FROM structs.api_refresh_state WHERE model <> 'inventory'`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE structs.api_refresh_state SET source_height=0,source_time=NULL
WHERE model='inventory'`)
	return err
}
