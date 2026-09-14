package readmodel

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConsumeDrift repairs structs.api_inventory from the latest
// structs.api_inventory_drift batch (written by the nightly reconciler)
// and deletes those drift rows. A non-empty batch two nights running
// means the incremental inventory path has a bug, not just a race.
//
// No-ops when the drift table is missing or empty. Never writes the
// reconciler function — it only consumes.
func ConsumeDrift(ctx context.Context, tx pgx.Tx) (int, error) {
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
SELECT owner_type::text, owner_id, denom
  FROM structs.api_inventory_drift
 WHERE checked_at = $1`, *checkedAt)
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
		if k.ownerType == "address" {
			if err := repairAddressInventory(ctx, tx, k.ownerID, k.denom); err != nil {
				return 0, err
			}
		}
	}
	seenPlayer := map[string]struct{}{}
	for _, k := range keys {
		if k.ownerType != "player" {
			continue
		}
		if _, ok := seenPlayer[k.ownerID]; ok {
			continue
		}
		seenPlayer[k.ownerID] = struct{}{}
		if err := rebuildPlayerInventory(ctx, tx, []string{k.ownerID}); err != nil {
			return 0, err
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM structs.api_inventory_drift WHERE checked_at = $1`, *checkedAt)
	if err != nil {
		return 0, fmt.Errorf("delete drift batch: %w", err)
	}
	_ = tag
	return len(keys), nil
}

func repairAddressInventory(ctx context.Context, tx pgx.Tx, address, denom string) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO structs.api_inventory (owner_type, owner_id, denom, balance)
SELECT 'address'::structs.object_type, $1, $2,
       COALESCE(SUM(CASE direction WHEN 'credit' THEN amount_p ELSE -amount_p END), 0)
  FROM structs.ledger
 WHERE address = $1 AND denom = $2
ON CONFLICT (owner_type, owner_id, denom)
DO UPDATE SET balance = EXCLUDED.balance`, address, denom); err != nil {
		return fmt.Errorf("repair address inventory %s %s: %w", address, denom, err)
	}
	return nil
}

// ConsumeDriftOn opens a short transaction against pool to apply the
// latest reconciler batch. Safe to call at ingest start.
func ConsumeDriftOn(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	n, err := ConsumeDrift(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}
