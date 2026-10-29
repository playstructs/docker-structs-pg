package staking

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/readmodel"
)

// Changes invalidates address evidence, not liquid balances. A validator pool
// mutation can also change other delegators' rounded balances. AllExposed is
// the conservative fallback for a financial slash whose consensus address
// cannot be resolved to an operator by the current indexed schema.
type Changes struct {
	Addresses  []string
	Validators []string
	AllExposed bool
}

// Request runs in the same transaction as the source event and cursor. It
// returns every affected address so the caller can invalidate its projections.
// Replaying an old event cannot make newer verified evidence incomplete.
func Request(ctx context.Context, tx pgx.Tx, chainID string, height int64, changes Changes) ([]string, error) {
	if chainID == "" || height <= 0 {
		return nil, fmt.Errorf("native invalidation requires chain and positive height")
	}
	addresses, err := uniqueAddresses(changes.Addresses, "structs1")
	if err != nil {
		return nil, err
	}
	validators, err := uniqueAddresses(changes.Validators, "structsvaloper1")
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 && len(validators) == 0 && !changes.AllExposed {
		return nil, nil
	}
	if err := readmodel.Lock(ctx, tx); err != nil {
		return nil, err
	}
	watermarks := append([]string{}, validators...)
	if changes.AllExposed {
		watermarks = append(watermarks, "")
	}
	if len(watermarks) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO sync_state.native_staking_invalidation(chain_id,validator,height)
SELECT $1,unnest($3::varchar[]),$2 ON CONFLICT (chain_id,validator) DO UPDATE
SET height=EXCLUDED.height WHERE sync_state.native_staking_invalidation.height<EXCLUDED.height`, chainID, height, watermarks); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `
WITH affected AS (
  SELECT unnest($3::varchar[]) AS address
  UNION
  SELECT address FROM sync_state.native_staking_exposure
  WHERE chain_id=$1 AND validator=ANY($4::varchar[])
  UNION
  SELECT address FROM sync_state.native_staking_exposure WHERE chain_id=$1 AND $5
)
INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height)
SELECT $1,address,$2 FROM affected
ON CONFLICT (chain_id,address) DO UPDATE
SET required_height=EXCLUDED.required_height, retry_after='-infinity'
WHERE sync_state.native_staking_snapshot.required_height < EXCLUDED.required_height
RETURNING address`, chainID, height, addresses, validators, changes.AllExposed)
	if err != nil {
		return nil, fmt.Errorf("invalidate native staking evidence: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Ensure records previously unseen candidates without invalidating an existing
// snapshot on an ordinary liquid transfer. Ownership changes must not turn a
// missing native query into a known zero. Run under the canonical write lock.
func Ensure(ctx context.Context, tx pgx.Tx, chainID string, height int64, addresses []string) error {
	if chainID == "" || height <= 0 {
		return fmt.Errorf("native candidate requires chain and positive height")
	}
	ids, err := uniqueAddresses(addresses, "structs1")
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if err := readmodel.Lock(ctx, tx); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height)
SELECT $1,unnest($3::varchar[]),$2 ON CONFLICT (chain_id,address) DO NOTHING`, chainID, height, ids)
	return err
}

type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type RequestItem struct {
	Address string
	Height  int64
}

// Pending selects a bounded batch at the committed indexed height near tip. Retry
// ordering keeps one failed/pruned address from starving unrelated accounts.
// HTTP work happens after this query, outside a database transaction/lock.
func Pending(ctx context.Context, q Queryer, chainID string, limit int) ([]RequestItem, error) {
	if chainID == "" || limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("native pending batch needs a chain and limit in 1..1000")
	}
	rows, err := q.Query(ctx, `
SELECT s.address,c.last_height
FROM sync_state.native_staking_snapshot s
JOIN sync_state.sync_cursor c ON c.chain_id=s.chain_id
WHERE s.chain_id=$1 AND (s.pending OR s.receipts_pending OR NOT s.positions_complete) AND s.retry_after<=NOW()
  AND s.required_height<=c.last_height AND c.last_height>0 AND c.status='current'
ORDER BY s.retry_after,s.address LIMIT $2`, chainID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, pgx.RowToStructByPos[RequestItem])
}

// Publish installs all totals and exposure together. The caller's transaction
// must also refresh affected projections before committing once they consume
// this authority. A newer cursor is safe only if no later event invalidated
// this address. Stale, unsolicited, future-height or wrong-chain results are
// refused without touching either the previous totals or exposure index.
func Publish(ctx context.Context, tx pgx.Tx, s *Snapshot) (bool, error) {
	if s == nil || s.ChainID == "" || s.Height <= 0 || !validAddress(s.Address, "structs1") ||
		!naturalTotal(s.Bonded) || !naturalTotal(s.Unbonding) {
		return false, fmt.Errorf("invalid complete native snapshot")
	}
	validators, err := uniqueAddresses(s.Validators(), "structsvaloper1")
	if err != nil {
		return false, err
	}
	positions, err := positionTotals(s)
	if err != nil {
		return false, err
	}
	if err := readmodel.Lock(ctx, tx); err != nil {
		return false, err
	}
	command, err := tx.Exec(ctx, `
UPDATE sync_state.native_staking_snapshot s
SET source_height=$3,bonded=$4::numeric,unbonding=$5::numeric,fetched_at=NOW(),retry_after='-infinity',receipts_pending=FALSE,positions_complete=TRUE
WHERE chain_id=$1 AND address=$2 AND required_height<=$3
  AND (source_height IS NULL OR source_height<$3 OR (source_height=$3 AND (receipts_pending OR NOT positions_complete)))
  AND EXISTS (SELECT 1 FROM sync_state.sync_cursor c WHERE c.chain_id=s.chain_id AND c.last_height>=$3)
  AND NOT EXISTS (SELECT 1 FROM sync_state.native_staking_invalidation i
      WHERE i.chain_id=s.chain_id AND (i.validator='' OR i.validator=ANY($6::varchar[])) AND i.height>$3)`,
		s.ChainID, s.Address, s.Height, s.Bonded, s.Unbonding, validators)
	if err != nil || command.RowsAffected() == 0 {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sync_state.native_staking_exposure WHERE chain_id=$1 AND address=$2`, s.ChainID, s.Address); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_state.native_staking_exposure(chain_id,validator,address)
SELECT $1,unnest($3::varchar[]),$2`, s.ChainID, s.Address, validators); err != nil {
		return false, err
	}
	if err := reconcileReceipts(ctx, tx, s); err != nil {
		return false, err
	}
	if err := replacePositions(ctx, tx, s, positions); err != nil {
		return false, err
	}
	return true, nil
}

// RetryLater only delays the request actually attempted. It preserves previous
// evidence as unknown and does not postpone a newer invalidation.
func RetryLater(ctx context.Context, tx pgx.Tx, chainID, address string, height int64, after time.Time) error {
	if err := readmodel.Lock(ctx, tx); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE sync_state.native_staking_snapshot SET retry_after=$4
WHERE chain_id=$1 AND address=$2 AND (pending OR receipts_pending OR NOT positions_complete) AND required_height<=$3`, chainID, address, height, after)
	return err
}

func uniqueAddresses(values []string, prefix string) ([]string, error) {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !validAddress(value, prefix) {
			return nil, fmt.Errorf("invalid native %s address %q", prefix, value)
		}
		seen[value] = true
	}
	ids := make([]string, 0, len(seen))
	for value := range seen {
		ids = append(ids, value)
	}
	sort.Strings(ids)
	return ids, nil
}

func naturalTotal(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
