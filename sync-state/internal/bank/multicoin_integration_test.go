package bank

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/buffers"
	"sync-state/internal/rpc"
)

func TestProcessBlock_MultiCoinReplayPreservesEveryDenomination(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx := context.Background()
		stream := []rpc.Event{{Type: "message"}, transferEvent("multicoin-sender", "multicoin-recipient", "18446744073709551615ualpha,9factory/issuer/token_name")}
		captured := Capture(990990, nil, []rpc.TxResult{{Events: stream}})
		for attempt := range 2 {
			buf := buffers.New()
			if err := ProcessBuffer(ctx, tx, buf, fixedTime(), "multi-coin-test", captured); err != nil {
				t.Fatal(err)
			}
			deltas, err := buf.Flush(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			if (attempt == 0 && len(deltas) != 4) || (attempt == 1 && len(deltas) != 0) {
				t.Fatalf("replay deltas=%+v", deltas)
			}
		}
		var count int
		var sum string
		if err := tx.QueryRow(ctx, `SELECT count(*),sum(CASE direction WHEN 'credit' THEN amount_p ELSE -amount_p END)::text FROM structs.ledger WHERE chain_id='multi-coin-test' AND block_height=990990`).Scan(&count, &sum); err != nil || count != 4 || sum != "0" {
			t.Fatalf("conservation count=%d sum=%s err=%v", count, sum, err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.ledger WHERE chain_id='multi-coin-test' AND event_index IN (8,9) AND msg_index=-1 AND tx_index=0`).Scan(&count); err != nil || count != 4 {
			t.Fatalf("original coordinates lost: %d %v", count, err)
		}
		var amount string
		if err := tx.QueryRow(ctx, `SELECT amount_p::text FROM structs.ledger WHERE chain_id='multi-coin-test' AND denom='ualpha' AND direction='credit'`).Scan(&amount); err != nil || amount != "18446744073709551615" {
			t.Fatalf("amount=%s err=%v", amount, err)
		}
	})
}
