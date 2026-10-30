package events

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/rpc"
)

func TestInfusionReconcile_CompleteSourceBoundary(t *testing.T) {
	for _, scenario := range []string{"generator", "mismatched snapshot", "wrong final coordinate", "missing row", "known owner mismatch", "deleted", "deletion mismatch", "unmatched source"} {
		t.Run(scenario, func(t *testing.T) {
			conn := connect(t)
			inTx(t, conn, func(tx pgx.Tx) {
				ctx := context.Background()
				bc := bctx()
				raw := mustJSON(t, map[string]any{
					"destinationId": "5-990997", "address": "reconcile", "destinationType": "struct",
					"playerId": "1-990997", "ownerId": "1-990997", "generatorDisabled": true,
					"fuel": "20", "defusing": "0", "power": "20", "ratio": "1", "commission": "0.04",
				})
				ev := rpc.Event{Type: "structs.structs.EventInfusion", Attributes: []rpc.Attribute{{Key: "infusion", Value: string(raw)}}}
				end := ev
				end.Attributes = append(append([]rpc.Attribute{}, ev.Attributes...), rpc.Attribute{Key: "mode", Value: "EndBlock"})
				ordered, err := rpc.OrderedEvents([]rpc.Event{end}, []rpc.TxResult{{Events: []rpc.Event{ev}}})
				if err != nil {
					t.Fatal(err)
				}
				bc.Phase, bc.TxIndex, bc.EventIndex = rpc.EndBlock, -1, 0
				handle(t, ctx, tx, infusionHandler{}, bc, raw)
				// Match the old writer: legacy fields and source coordinates were
				// retained, but phase and v0.22 extensions were not understood.
				if _, err := tx.Exec(ctx, `UPDATE sync_state.infusion_event_position SET phase=NULL
WHERE destination_id='5-990997' AND address='reconcile';
UPDATE structs.infusion SET owner_id=NULL,generator_disabled=FALSE
WHERE destination_id='5-990997' AND address='reconcile'`); err != nil {
					t.Fatal(err)
				}
				var mutate string
				switch scenario {
				case "unmatched source":
					ordered = nil
				case "mismatched snapshot":
					mutate = `UPDATE structs.infusion SET fuel_p=21 WHERE destination_id='5-990997' AND address='reconcile'`
				case "wrong final coordinate":
					mutate = `UPDATE sync_state.infusion_event_position SET tx_index=0 WHERE destination_id='5-990997' AND address='reconcile'`
				case "missing row":
					mutate = `DELETE FROM structs.infusion WHERE destination_id='5-990997' AND address='reconcile'`
				case "known owner mismatch":
					mutate = `UPDATE structs.infusion SET owner_id='1-990998' WHERE destination_id='5-990997' AND address='reconcile'`
				case "deleted", "deletion mismatch":
					ordered = append(ordered, rpc.PositionedEvent{Phase: rpc.EndBlock, TxIndex: -1, EventIndex: 1,
						Event: rpc.Event{Type: "structs.structs.EventDelete", Attributes: []rpc.Attribute{{Key: "objectId", Value: `"5-990997-reconcile"`}}}})
					mutate = `UPDATE sync_state.infusion_event_position SET event_index=1 WHERE destination_id='5-990997' AND address='reconcile'`
					if scenario == "deleted" {
						mutate += `; DELETE FROM structs.infusion WHERE destination_id='5-990997' AND address='reconcile'`
					}
				}
				if mutate != "" {
					if _, err := tx.Exec(ctx, mutate); err != nil {
						t.Fatal(err)
					}
				}
				n, err := ReconcileLegacyInfusionPositions(ctx, tx, bc, ordered)
				wantSuccess := scenario == "generator" || scenario == "deleted"
				if !wantSuccess {
					if err == nil {
						t.Fatal("adopted a phase without matching complete source evidence")
					}
					return
				}
				if err != nil || n != 1 {
					t.Fatalf("reconciled=%d err=%v", n, err)
				}
				if n, err = ReconcileLegacyInfusionPositions(ctx, tx, bc, ordered); err != nil || n != 0 {
					t.Fatalf("repeat reconciliation: count=%d err=%v", n, err)
				}
				if scenario == "generator" {
					for _, source := range ordered {
						bc.Phase, bc.TxIndex, bc.EventIndex = source.Phase, source.TxIndex, source.EventIndex
						if err := (infusionHandler{}).Handle(ctx, tx, bc, raw); err != nil {
							t.Fatal(err)
						}
					}
					if len(bc.Buf.Ledger) != 0 {
						t.Fatal("reconciled generator emitted a duplicate fuel delta")
					}
					var owner string
					var disabled bool
					if err := tx.QueryRow(ctx, `SELECT owner_id,generator_disabled FROM structs.infusion
WHERE destination_id='5-990997' AND address='reconcile'`).Scan(&owner, &disabled); err != nil || owner != "1-990997" || !disabled {
						t.Fatalf("extensions not restored: %s %v %v", owner, disabled, err)
					}
				}
			})
		})
	}
}
