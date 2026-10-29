package verify

import (
	"context"
	"sync-state/internal/staking"
	"testing"
)

func TestCheckPendingReceipts_UnknownMissingWorkAndRetiredBoundary(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	const chain = "verify-receipts"
	const address = "structs1q3nx9j4qcqaf4p2qjc9zxqxrccg4renc2fyaw5"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sync_state.sync_cursor(chain_id,last_height,last_block_time) VALUES($1,100,NOW())`, chain)
	exec(`INSERT INTO structs.defusion(chain_id,block_height,tx_index,event_index,validator_address,delegator_address,defusion_type,denom,amount_p,completed_at,created_at)
VALUES($1,50,0,1,'structsvaloper1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq',$2,'u','ualpha',1,NOW(),NOW())`, chain, address)
	check := func(want Status) {
		t.Helper()
		r := checkPendingReceipts(ctx, Inputs{Pool: tx, ChainID: chain})
		if r.Status != want {
			t.Fatalf("got=%s want=%s detail=%s counts=%v", r.Status, want, r.Detail, r.Counts)
		}
	}
	check(StatusFail)
	exec(`INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height,receipts_pending) VALUES($1,$2,50,TRUE)`, chain, address)
	check(StatusInfo)
	if ok, err := staking.Publish(ctx, tx, &staking.Snapshot{ChainID: chain, Address: address, Height: 100, Bonded: "0", Unbonding: "0"}); err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
	check(StatusPass)
	exec(`UPDATE structs.defusion SET reconciled_height=101,retired_height=101 WHERE chain_id=$1`, chain)
	check(StatusFail)
}
