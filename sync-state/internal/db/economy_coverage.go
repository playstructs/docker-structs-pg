package db

import (
	"context"
	"fmt"
	"strings"

	"sync-state/internal/protocol"
)

// RecordEconomyCoverage is committed with the complete authoritative block.
// Coverage starts at the upgrade itself and cannot jump a missing range, even
// when an old writer has advanced sync_cursor beyond it. Same-block retry is
// permitted only with the same hash.
func RecordEconomyCoverage(ctx context.Context, q Querier, chainID string, height int64, hash string) error {
	if !protocol.V022Active(chainID, height) {
		return nil
	}
	result, err := q.Exec(ctx, `INSERT INTO sync_state.economy_contract
(chain_id,contract,activation_height,last_height,last_block_hash)
SELECT $1::text,'events-v022-1',$2::bigint,$3::bigint,$4::text
WHERE $3=$2 OR EXISTS(SELECT 1 FROM sync_state.economy_contract
 WHERE chain_id=$1 AND contract='events-v022-1' AND activation_height=$2
 AND (last_height=$3-1 OR (last_height=$3 AND last_block_hash=$4)))
ON CONFLICT(chain_id) DO UPDATE SET last_height=EXCLUDED.last_height,
last_block_hash=EXCLUDED.last_block_hash,updated_at=NOW()
WHERE economy_contract.contract=EXCLUDED.contract
AND economy_contract.activation_height=EXCLUDED.activation_height
AND (economy_contract.last_height=EXCLUDED.last_height-1
 OR (economy_contract.last_height=EXCLUDED.last_height AND economy_contract.last_block_hash=EXCLUDED.last_block_hash))`,
		chainID, protocol.V022TestnetHeight, height, strings.ToUpper(hash))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("missing contiguous v0.22 event coverage before height %d; restore and reconcile a complete activation boundary before resuming", height)
	}
	return nil
}

// RequireEconomyCoverage runs before any startup projection rebuild. The SQL
// API independently enforces the same watermark, including after rollback to
// an older writer which does not know to advance it.
func RequireEconomyCoverage(ctx context.Context, q Querier, cursor Cursor) error {
	if !protocol.V022Active(cursor.ChainID, cursor.LastHeight) {
		return nil
	}
	var ready bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_state.economy_contract
WHERE chain_id=$1 AND contract='events-v022-1' AND activation_height=$2
AND last_height=$3 AND last_block_hash=$4)`, cursor.ChainID, protocol.V022TestnetHeight,
		cursor.LastHeight, strings.ToUpper(cursor.LastBlockHash)).Scan(&ready); err != nil {
		return err
	}
	if !ready {
		if cursor.LastHeight == protocol.V022TestnetHeight {
			return fmt.Errorf("upgrade block was not fully processed by the v0.22 writer; run sync-state reconcile-current-block -start=%d before ingestion", cursor.LastHeight)
		}
		return fmt.Errorf("indexed cursor has no matching v0.22 source coverage; restore a complete activation boundary and reconcile before catch-up")
	}
	return nil
}
