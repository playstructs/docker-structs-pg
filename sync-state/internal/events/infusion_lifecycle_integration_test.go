package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/protocol"
)

func TestInfusion_OwnerDisableDeleteAndReplay(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx, bc := context.Background(), bctx()
		bc.ChainID, bc.Height = "structstestnet-111", protocol.V022TestnetHeight
		p := map[string]any{"destinationId": "5-990994", "address": "structs1lifecycle", "destinationType": "struct",
			"playerId": "1-990994", "ownerId": "1-990994", "generatorDisabled": false,
			"fuel": "1000000", "defusing": "0", "power": "1000", "ratio": "1", "commission": "0"}
		apply := func() {
			if err := (infusionHandler{}).Handle(ctx, tx, bc, mustJSON(t, p)); err != nil {
				t.Fatal(err)
			}
		}
		apply()
		initialLegs := len(bc.Buf.Ledger)
		bc.EventIndex++
		p["generatorDisabled"], p["power"] = true, "0"
		apply()
		bc.EventIndex++
		p["generatorDisabled"], p["power"] = false, "1000"
		p["ownerId"], p["playerId"] = "0-990994", ""
		apply()
		var owner, fuel string
		var disabled bool
		if err := tx.QueryRow(ctx, `SELECT owner_id,fuel_p::text,generator_disabled FROM structs.infusion WHERE destination_id='5-990994' AND address='structs1lifecycle'`).Scan(&owner, &fuel, &disabled); err != nil || owner != "0-990994" || fuel != "1000000" || disabled {
			t.Fatalf("re-enabled/re-homed position=%s %s %v err=%v", owner, fuel, disabled, err)
		}
		bc.EventIndex++
		for range 2 {
			if err := (deleteHandler{}).Handle(ctx, tx, bc, json.RawMessage(`"5-990994-structs1lifecycle"`)); err != nil {
				t.Fatal(err)
			}
		}
		bc.EventIndex--
		apply() // older snapshot must not resurrect the position
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.infusion WHERE destination_id='5-990994' AND address='structs1lifecycle'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("position survived deletion: %d %v", count, err)
		}
		if len(bc.Buf.Ledger) != initialLegs {
			t.Fatal("disable, reassignment or deletion fabricated a financial movement")
		}
		if len(bc.Dirty.Players) != 1 || len(bc.Dirty.Guilds) != 1 || len(bc.Dirty.Addresses) != 1 {
			t.Fatal("old/new owner and address were not invalidated")
		}
		if err := (deleteHandler{}).Handle(ctx, tx, bc, json.RawMessage(`"3-4-address-extra"`)); err == nil {
			t.Fatal("malformed composite deletion was ignored")
		}
	})
}

func TestInfusion_RequiresExtensionAtActivation(t *testing.T) {
	bc := bctx()
	bc.ChainID, bc.Height = "structstestnet-111", protocol.V022TestnetHeight
	if err := (infusionHandler{}).Handle(context.Background(), nil, bc, json.RawMessage(`{"destinationId":"3-1","address":"structs1x"}`)); err == nil {
		t.Fatal("current payload without required fields accepted")
	}
}
