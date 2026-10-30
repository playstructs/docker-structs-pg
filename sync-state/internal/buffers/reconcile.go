package buffers

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ValidateExistingLedger checks the contents behind replay identities before
// ON CONFLICT can hide a disagreement. Missing denomination legs are allowed;
// replacing an existing financial fact is not. This maintenance-only staging
// table avoids one round trip per row in a large upgrade block.
func (b *Buffer) ValidateExistingLedger(ctx context.Context, tx pgx.Tx) error {
	if len(b.Ledger) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE sync_reconcile_ledger (
time timestamptz, address text, counterparty text, amount_p numeric,
block_height bigint, action text, direction text, denom text, chain_id text,
tx_index integer, msg_index integer, event_index integer) ON COMMIT DROP`); err != nil {
		return err
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"pg_temp", "sync_reconcile_ledger"}, []string{
		"time", "address", "counterparty", "amount_p", "block_height", "action", "direction", "denom",
		"chain_id", "tx_index", "msg_index", "event_index",
	}, pgx.CopyFromSlice(len(b.Ledger), func(i int) ([]any, error) {
		r := b.Ledger[i]
		var amount pgtype.Numeric
		if err := amount.Scan(nullable(r.AmountP)); err != nil {
			return nil, err
		}
		return []any{r.Time, r.Address, nullable(r.Counterparty), amount, r.BlockHeight, r.Action,
			r.Direction, r.Denom, r.ChainID, r.TxIndex, r.MsgIndex, r.EventIndex}, nil
	}))
	if err != nil {
		return err
	}
	var mismatches int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_temp.sync_reconcile_ledger expected
JOIN structs.ledger stored USING(time,chain_id,tx_index,msg_index,event_index,denom)
WHERE ROW(stored.address,stored.counterparty,stored.amount_p,stored.block_height,stored.action::text,stored.direction::text)
IS DISTINCT FROM ROW(expected.address,expected.counterparty,expected.amount_p,expected.block_height,expected.action,expected.direction)`).Scan(&mismatches); err != nil {
		return err
	}
	if mismatches != 0 {
		return fmt.Errorf("%d existing ledger rows disagree with the complete source block", mismatches)
	}
	_, err = tx.Exec(ctx, `DROP TABLE pg_temp.sync_reconcile_ledger`)
	return err
}
