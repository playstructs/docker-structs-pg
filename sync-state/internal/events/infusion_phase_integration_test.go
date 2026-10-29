package events

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/rpc"
)

func TestInfusion_EndBlockSupersedesTransaction(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		apply := func(phase rpc.ExecutionPhase, txIndex, eventIndex int, power string) {
			bc := bctx()
			bc.Phase, bc.TxIndex, bc.EventIndex = phase, txIndex, eventIndex
			handle(t, ctx, tx, infusionHandler{}, bc, mustJSON(t, map[string]string{
				"destinationId": "3-990999", "destinationType": "reactor", "address": "phase-test",
				"playerId": "1-990999", "fuel": "1", "defusing": "0", "power": power, "ratio": "1", "commission": "0",
			}))
		}
		apply(rpc.PreBlock, -1, 0, "10")
		apply(rpc.Transaction, 0, 10, "20")
		apply(rpc.EndBlock, -1, 1, "30")
		apply(rpc.Transaction, 0, 10, "20") // historical retry cannot resurrect it
		var power string
		if err := tx.QueryRow(ctx, `SELECT power_p::text FROM structs.infusion WHERE destination_id='3-990999' AND address='phase-test'`).Scan(&power); err != nil {
			t.Fatal(err)
		}
		if power != "30" {
			t.Fatalf("EndBlock power lost: %s", power)
		}
	})
}

func TestInfusion_LegacyPhaseRequiresKnownBoundary(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `INSERT INTO sync_state.infusion_event_position(destination_id,address,height,tx_index,event_index) VALUES ('3-990998','legacy-phase',100,-1,0)`); err != nil {
			t.Fatal(err)
		}
		raw := mustJSON(t, map[string]string{"destinationId": "3-990998", "destinationType": "reactor", "address": "legacy-phase", "fuel": "0", "defusing": "0", "power": "0", "ratio": "1", "commission": "0"})
		bc := bctx()
		if err := (infusionHandler{}).Handle(ctx, tx, bc, raw); err == nil {
			t.Fatal("guessed unknown same-height phase")
		}
		bc.Height++
		handle(t, ctx, tx, infusionHandler{}, bc, raw)
	})
}
