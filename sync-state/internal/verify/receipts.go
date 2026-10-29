package verify

import (
	"context"
	"fmt"
)

// This checks receipt boundaries and reconciliation coverage, not current
// balances or native entry IDs. Lifecycle correctness is exercised separately
// against complete native snapshots and replayed source events.
func checkPendingReceipts(ctx context.Context, in Inputs) CheckResult {
	r := CheckResult{Counts: map[string]any{}}
	var retired, unidentified, unknown, pending, missing, bad int64
	err := in.Pool.QueryRow(ctx, `SELECT
count(*) FILTER(WHERE d.retired_height IS NOT NULL),
count(*) FILTER(WHERE d.retired_height IS NULL AND d.block_height IS NULL),
count(*) FILTER(WHERE d.retired_height IS NULL AND d.block_height IS NOT NULL
    AND NOT COALESCE(s.complete AND s.source_height=d.reconciled_height,FALSE)),
count(*) FILTER(WHERE d.retired_height IS NULL AND d.block_height IS NOT NULL
    AND s.complete AND s.source_height=d.reconciled_height),
count(*) FILTER(WHERE d.retired_height IS NULL AND d.chain_id IS NOT NULL AND d.denom='ualpha'
    AND d.reconciled_height IS NULL AND NOT COALESCE(n.pending OR n.receipts_pending,FALSE)),
count(*) FILTER(WHERE d.chain_id IS NOT NULL AND (c.last_height IS NULL
    OR COALESCE(d.block_height,d.legacy_observed_height)>c.last_height
    OR d.reconciled_height>c.last_height))
FROM structs.defusion d
LEFT JOIN sync_state.native_staking_snapshot n ON n.chain_id=d.chain_id AND n.address=d.delegator_address
LEFT JOIN sync_state.native_staking_claim s ON s.chain_id=d.chain_id AND s.address=d.delegator_address
LEFT JOIN sync_state.sync_cursor c ON c.chain_id=d.chain_id
WHERE d.chain_id=$1 OR d.chain_id IS NULL`, in.ChainID).Scan(&retired, &unidentified, &unknown, &pending, &missing, &bad)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("pending receipt check: %v", err)
		return r
	}
	r.Counts = map[string]any{"retired_receipts": retired, "unidentified_receipts": unidentified,
		"unknown_receipts": unknown, "matched_receipts": pending, "missing_requests": missing, "invalid_boundaries": bad}
	switch {
	case missing > 0 || bad > 0:
		r.Status = StatusFail
		r.Detail = "receipt reconciliation work or indexed boundary is inconsistent"
	case unknown > 0 || unidentified > 0:
		r.Status = StatusInfo
		r.Detail = "receipt reconciliation is incomplete; receipt amounts are not current pending balances"
	default:
		r.Status = StatusPass
		r.Detail = "receipt boundaries and reconciliation coverage agree; retired receipts remain replay tombstones"
	}
	return r
}
