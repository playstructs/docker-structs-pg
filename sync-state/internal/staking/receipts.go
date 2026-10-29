package staking

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/readmodel"
)

// SeedReceipts captures a source upper bound for unidentified legacy receipts
// only when this is the sole indexed chain, under the canonical write lock.
// It preserves duplicates and never invents event coordinates or native IDs.
func SeedReceipts(ctx context.Context, tx pgx.Tx, chainID string, height int64) error {
	if chainID == "" || height <= 0 {
		return fmt.Errorf("legacy receipt adoption requires chain and positive height")
	}
	if err := readmodel.Lock(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE structs.defusion
SET chain_id=$1,legacy_observed_height=$2
WHERE chain_id IS NULL AND retired_height IS NULL
  AND (SELECT count(*) FROM sync_state.sync_cursor)=1
  AND EXISTS (SELECT 1 FROM sync_state.sync_cursor WHERE chain_id=$1 AND last_height=$2)`, chainID, height); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height,receipts_pending)
SELECT chain_id,delegator_address,MAX(COALESCE(block_height,legacy_observed_height)),TRUE
FROM structs.defusion WHERE chain_id=$1 AND retired_height IS NULL AND reconciled_height IS NULL
  AND delegator_address ~ '^structs1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]{1,121}$'
GROUP BY chain_id,delegator_address
ON CONFLICT (chain_id,address) DO UPDATE SET receipts_pending=TRUE,retry_after='-infinity',
    required_height=GREATEST(sync_state.native_staking_snapshot.required_height,EXCLUDED.required_height)`, chainID)
	return err
}

type receiptGroup struct {
	Kind       string `json:"kind"`
	Validator  string `json:"validator"`
	Source     string `json:"source"`
	Height     int64  `json:"height"`
	Completion string `json:"completion"`
}

// Reconcile only after Publish accepts the complete snapshot. Receipt identity
// is not entry identity: SDK merges same-block unbonds, and ordinary events do
// not expose the native ID. Retire a receipt only when its entire compatible
// group is absent. Never distribute a slashed/cancelled group balance across
// receipts or infer absence from amount, maturity, or the query's hold count.
func reconcileReceipts(ctx context.Context, tx pgx.Tx, s *Snapshot) error {
	groups := make([]receiptGroup, 0, len(s.Unbondings)+len(s.Redelegations))
	for _, u := range s.Unbondings {
		groups = append(groups, receiptGroup{Kind: "u", Validator: u.Validator, Height: u.CreationHeight,
			Completion: u.CompletionTime.UTC().Truncate(time.Second).Format(time.RFC3339)})
	}
	for _, r := range s.Redelegations {
		groups = append(groups, receiptGroup{Kind: "r", Validator: r.DestinationValidator, Source: r.SourceValidator,
			Height: r.CreationHeight, Completion: r.CompletionTime.UTC().Truncate(time.Second).Format(time.RFC3339)})
	}
	raw, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	// Redelegation creation_height can be the source validator's unbonding
	// height, not the event height. Match that group by endpoints/completion.
	// SDK events format completion to whole seconds; LCD preserves nanos.
	_, err = tx.Exec(ctx, `WITH groups AS MATERIALIZED (
SELECT * FROM jsonb_to_recordset($4::jsonb) AS g(kind text,validator text,source text,height bigint,completion timestamptz)
)
UPDATE structs.defusion d SET reconciled_height=$3,retired_height=CASE WHEN EXISTS (
    SELECT 1 FROM groups g WHERE g.kind=d.defusion_type AND g.validator=d.validator_address
      AND (d.defusion_type<>'r' OR d.source_validator_address IS NULL OR g.source=d.source_validator_address)
      AND (d.block_height IS NULL OR d.defusion_type='r' OR g.height=d.block_height)
      AND date_trunc('second',d.completed_at)=g.completion
) THEN NULL ELSE $3 END
WHERE d.chain_id=$1 AND d.delegator_address=$2 AND d.retired_height IS NULL AND d.denom='ualpha'
  AND COALESCE(d.block_height,d.legacy_observed_height)<=$3
  AND (d.reconciled_height IS NULL OR d.reconciled_height<=$3)
  AND d.defusion_type IN ('u','r') AND d.validator_address IS NOT NULL AND d.completed_at IS NOT NULL`, s.ChainID, s.Address, s.Height, raw)
	return err
}
