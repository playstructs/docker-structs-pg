package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// activity_helpers contains the Go ports of two PG helpers used by the
// Phase 5 attack handler (and by any future handler that wants to write
// to structs.planet_activity).
//
//   - structs.GET_ACTIVITY_LOCATION_ID(struct_id) → planet id
//     (table-struct.sql:39-53)
//   - structs.GET_PLANET_ACTIVITY_SEQUENCE(planet_id) → next seq
//     (table-planet.sql:61-74)
//
// We port them to Go because they're tiny, frequently called, and writing
// them inline removes a function-call layer from PG's POV (the cache
// handlers historically called them via SELECT-subqueries).

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

// nextPlanetActivitySeqSQL ports structs.GET_PLANET_ACTIVITY_SEQUENCE.
// Upsert+increment in one round trip, returning the new counter value.
const nextPlanetActivitySeqSQL = `
INSERT INTO structs.planet_activity_sequence (planet_id, counter)
VALUES ($1, 0)
ON CONFLICT (planet_id) DO UPDATE
   SET counter = planet_activity_sequence.counter + 1
RETURNING counter`

// nextPlanetActivitySeq atomically increments the per-planet activity
// counter and returns the new value. Returns 0 on the first call for a
// given planet (matching the SQL: VALUES($1,0) inserts 0, no UPDATE).
func nextPlanetActivitySeq(ctx context.Context, tx pgx.Tx, planetID string) (int, error) {
	if planetID == "" {
		return 0, errors.New("nextPlanetActivitySeq: empty planet_id")
	}
	var seq int
	if err := tx.QueryRow(ctx, nextPlanetActivitySeqSQL, planetID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("planet_activity_sequence planet=%s: %w", planetID, err)
	}
	return seq, nil
}
