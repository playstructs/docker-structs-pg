package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const activityLocationStructSQL = `
SELECT location_type, location_id FROM structs.struct WHERE id = $1`

const activityLocationFleetSQL = `
SELECT location_id FROM structs.fleet WHERE id = $1`

// getActivityLocationID returns the planet id that owns the activity for
// the given struct. If the struct sits on a fleet, the fleet's location_id
// is returned instead (mirrors GET_ACTIVITY_LOCATION_ID).
//
// Returns ("", nil) when the struct doesn't exist — the SQL function also
// returns NULL in that case and the caller (e.g. attack) inserts a row
// with NULL planet_id. We mirror that exactly.
func getActivityLocationID(ctx context.Context, tx pgx.Tx, structID string) (string, error) {
	if structID == "" {
		return "", nil
	}
	var locType, locID string
	err := tx.QueryRow(ctx, activityLocationStructSQL, structID).Scan(&locType, &locID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("activity_location struct=%s: %w", structID, err)
	}
	if locType != "fleet" {
		return locID, nil
	}
	// Struct is parked on a fleet — resolve to the fleet's location.
	var fleetLoc string
	err = tx.QueryRow(ctx, activityLocationFleetSQL, locID).Scan(&fleetLoc)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("activity_location fleet=%s: %w", locID, err)
	}
	return fleetLoc, nil
}
