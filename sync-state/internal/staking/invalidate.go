package staking

import (
	"context"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/protocol"
	"sync-state/internal/readmodel"
)

// Invalidate combines native SDK events with applied Structs snapshots/deletes
// before projections and the cursor commit. It also discovers newly associated
// addresses without requiring a bank receipt. No LCD request runs in this tx.
func Invalidate(ctx context.Context, tx pgx.Tx, chainID string, height int64, dirty *readmodel.Dirty, changes Changes) error {
	if protocol.V022Active(chainID, height) && height == protocol.V022TestnetHeight {
		// The upgrade repairs native delegation accounting as well as game
		// state. Prior-height evidence cannot certify the repaired claims.
		changes.AllExposed = true
	}
	for address := range dirty.NativeAddresses {
		changes.Addresses = append(changes.Addresses, address)
	}
	if len(dirty.NativeReactors) > 0 {
		ids := make([]string, 0, len(dirty.NativeReactors))
		for id := range dirty.NativeReactors {
			ids = append(ids, id)
		}
		rows, err := tx.Query(ctx, `SELECT r.validator FROM unnest($1::varchar[]) AS wanted(id)
LEFT JOIN structs.reactor r ON r.id=wanted.id`, ids)
		if err != nil {
			return err
		}
		for rows.Next() {
			var validator *string
			if err := rows.Scan(&validator); err != nil {
				rows.Close()
				return err
			}
			if validator == nil || *validator == "" {
				changes.AllExposed = true
			} else {
				changes.Validators = append(changes.Validators, *validator)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	addresses, err := Request(ctx, tx, chainID, height, changes)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		dirty.Address(address)
	}
	rows, err := tx.Query(ctx, `SELECT address FROM structs.asset_owner
WHERE address=ANY($1::varchar[]) OR owner_id=ANY($2::varchar[]) OR owner_id=ANY($3::varchar[])`,
		dirty.AddressIDs(), dirty.PlayerIDs(), dirty.GuildIDs())
	if err != nil {
		return err
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return Ensure(ctx, tx, chainID, height, candidates)
}
