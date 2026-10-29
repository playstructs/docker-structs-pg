package readmodel

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConsumeDrift repairs structs.api_inventory from outstanding
// structs.api_inventory_drift batches (written by the nightly reconciler)
// and deletes those drift rows. A non-empty batch two nights running
// means the incremental inventory path has a bug, not just a race.
//
// No-ops when the drift table is missing or empty. Never writes the
// reconciler function — it only consumes.
func ConsumeDrift(ctx context.Context, tx pgx.Tx) (int, error) {
	return consumeDrift(ctx, tx, NewDirty())
}

func consumeDrift(ctx context.Context, tx pgx.Tx, dirty *Dirty) (int, error) {
	if err := Lock(ctx, tx); err != nil {
		return 0, err
	}
	var present *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('structs.api_inventory_drift')`).Scan(&present); err != nil {
		return 0, fmt.Errorf("lookup api_inventory_drift: %w", err)
	}
	if present == nil {
		return 0, nil
	}
	var checkedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT MAX(checked_at) FROM structs.api_inventory_drift`).Scan(&checkedAt); err != nil {
		return 0, fmt.Errorf("latest drift batch: %w", err)
	}
	if checkedAt == nil {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `
SELECT DISTINCT owner_type::text, owner_id, denom
  FROM structs.api_inventory_drift
 WHERE checked_at <= $1`, *checkedAt)
	if err != nil {
		return 0, fmt.Errorf("read drift batch: %w", err)
	}
	type key struct{ ownerType, ownerID, denom string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.ownerType, &k.ownerID, &k.denom); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, k := range keys {
		switch k.ownerType {
		case "address":
			dirty.Address(k.ownerID)
		case "player":
			dirty.Player(k.ownerID)
		case "guild":
			dirty.Guild(k.ownerID)
		}
	}
	dirty.inventoryPlayers = cloneSet(dirty.Players)
	if err := expandAddresses(ctx, tx, dirty); err != nil {
		return 0, err
	}
	if err := recomputeInventory(ctx, tx, dirty); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `DELETE FROM structs.api_inventory_drift WHERE checked_at <= $1`, *checkedAt)
	if err != nil {
		return 0, fmt.Errorf("delete drift batch: %w", err)
	}
	return len(keys), nil
}

// ConsumeDriftOn opens a short transaction against pool to apply the
// outstanding reconciler batches. Safe to call at ingest start.
func ConsumeDriftOn(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	n, err := consumeDriftReady(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

// consumeDriftReady repairs only a complete indexed snapshot and refreshes its
// dependent models before returning. The caller commits the same transaction.
func consumeDriftReady(ctx context.Context, tx pgx.Tx) (int, error) {
	if err := Lock(ctx, tx); err != nil {
		return 0, err
	}
	var height int64
	var sourceTime *time.Time
	if err := tx.QueryRow(ctx, `SELECT b.source_height, b.source_time FROM structs.api_projection_boundary() b
WHERE (SELECT count(*) FROM structs.api_refresh_state rs
        WHERE rs.model=ANY($1::text[]) AND rs.source_height=b.source_height
          AND rs.source_time IS NOT DISTINCT FROM b.source_time)=$2`, modelNames, len(modelNames)).Scan(&height, &sourceTime); err != nil {
		if err == pgx.ErrNoRows {
			// Deferred/uninitialized projections need a full backfill, not a
			// partial repair that would advance their readiness watermark.
			return 0, nil
		}
		return 0, err
	}
	dirty := NewDirty()
	n, err := consumeDrift(ctx, tx, dirty)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if err := Recompute(ctx, tx, dirty, height, sourceTime, InventoryAlreadyWritten()); err != nil {
			return 0, fmt.Errorf("refresh repaired inventory dependencies: %w", err)
		}
	}
	return n, nil
}
