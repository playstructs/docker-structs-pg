package staking

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPendingMatchesUnionOfEligibleWork(t *testing.T) {
	ctx, tx := storeTx(t)
	// Mix overlapping flags, completed rows, future heights and delayed retries.
	// More than 1000 eligible rows exercises branch limits with a full backlog.
	_, err := tx.Exec(ctx, `INSERT INTO sync_state.native_staking_snapshot
 (chain_id,address,required_height,source_height,receipts_pending,positions_complete,retry_after,bonded,unbonding,fetched_at)
 SELECT $1,'structs1queue'||lpad(i::text,5,'0'),CASE WHEN i%11=0 THEN 201 ELSE 100 END,
 CASE WHEN i%2=0 THEN NULL ELSE 100 END,i%3=0,i%5<>0,
 CASE WHEN i%7=0 THEN NOW()+interval '1 day' ELSE NOW()-interval '1 day'+(i%13)*interval '1 second' END,
 CASE WHEN i%2=0 THEN NULL ELSE 0 END, CASE WHEN i%2=0 THEN NULL ELSE 0 END, CASE WHEN i%2=0 THEN NULL ELSE NOW() END
 FROM generate_series(1,3000) i`, storeChain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SET LOCAL plan_cache_mode=force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	old := `SELECT s.address,c.last_height FROM sync_state.native_staking_snapshot s
 JOIN sync_state.sync_cursor c ON c.chain_id=s.chain_id
 WHERE s.chain_id=$1 AND c.last_height>0 AND c.status='current'
 AND (s.pending OR s.receipts_pending OR NOT s.positions_complete)
 AND s.retry_after<=NOW() AND s.required_height<=c.last_height
 ORDER BY s.retry_after,s.address LIMIT $2`
	for _, limit := range []int{1, 8, 1000} {
		rows, err := tx.Query(ctx, old, storeChain, limit)
		if err != nil {
			t.Fatal(err)
		}
		want, err := pgx.CollectRows(rows, pgx.RowToStructByPos[RequestItem])
		if err != nil {
			t.Fatal(err)
		}
		got, err := Pending(ctx, tx, storeChain, limit)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("limit %d: result/order differs", limit)
		}
		if len(got) != limit {
			t.Fatalf("fixture has insufficient eligible rows: %d", len(got))
		}
	}
	for _, status := range []string{"catching_up", "current"} {
		if _, err = tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET status=$2,last_height=CASE WHEN $2='current' THEN 0 ELSE 200 END WHERE chain_id=$1`, storeChain, status); err != nil {
			t.Fatal(err)
		}
		got, err := Pending(ctx, tx, storeChain, 8)
		if err != nil || len(got) != 0 {
			t.Fatalf("non-ready cursor returned work: %v %v", got, err)
		}
	}
	got, err := Pending(ctx, tx, "absent-chain", 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("wrong chain: %v %v", got, err)
	}
	if _, err = tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=200,status='current' WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE sync_state.native_staking_snapshot SET source_height=required_height,bonded=0,unbonding=0,fetched_at=NOW(),receipts_pending=false,positions_complete=true WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	got, err = Pending(ctx, tx, storeChain, 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("drained queue: %v %v", got, err)
	}
}
