package staking

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPositions_LegacyRefreshAtomicReplacementAndCompletion(t *testing.T) {
	ctx, tx := storeTx(t)
	// A previously complete address total does not establish per-validator
	// evidence. It must be refreshed, even at the very same indexed height.
	if _, err := tx.Exec(ctx, `INSERT INTO sync_state.native_staking_snapshot
(chain_id,address,required_height,source_height,bonded,unbonding,fetched_at)
VALUES ($1,$2,100,200,18446744073709551616,7,NOW())`, storeChain, testAddress); err != nil {
		t.Fatal(err)
	}
	assert := func(q pgx.Tx, complete bool, rows int, bonded, unbonding string, entries int) {
		t.Helper()
		var gotComplete bool
		var gotRows, gotEntries int
		var gotBonded, gotUnbonding string
		if err := q.QueryRow(ctx, `SELECT complete FROM sync_state.native_staking_position_state
WHERE chain_id=$1 AND address=$2`, storeChain, testAddress).Scan(&gotComplete); err != nil {
			t.Fatal(err)
		}
		if err := q.QueryRow(ctx, `SELECT count(*),coalesce(sum(bonded),0)::text,coalesce(sum(unbonding),0)::text,
coalesce(sum(unbonding_entries),0)::int FROM sync_state.native_staking_position WHERE chain_id=$1 AND address=$2`, storeChain, testAddress).
			Scan(&gotRows, &gotBonded, &gotUnbonding, &gotEntries); err != nil {
			t.Fatal(err)
		}
		if gotComplete != complete || gotRows != rows || gotBonded != bonded || gotUnbonding != unbonding || gotEntries != entries {
			t.Fatalf("complete=%v rows=%d bonded=%s pending=%s entries=%d", gotComplete, gotRows, gotBonded, gotUnbonding, gotEntries)
		}
	}
	assert(tx, false, 0, "0", "0", 0)
	items, err := Pending(ctx, tx, storeChain, 1)
	if err != nil || len(items) != 1 || items[0].Address != testAddress {
		t.Fatalf("legacy positions were not queued: %v %v", items, err)
	}
	if err := RetryLater(ctx, tx, storeChain, testAddress, 200, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if items, err := Pending(ctx, tx, storeChain, 1); err != nil || len(items) != 0 {
		t.Fatalf("position-only retry ignored: %v %v", items, err)
	}
	s := &Snapshot{ChainID: storeChain, Address: testAddress, Height: 200, Bonded: "18446744073709551616", Unbonding: "7",
		Delegations:   []Delegation{{Validator: validatorA, Balance: "18446744073709551616"}},
		Unbondings:    []Unbonding{{Validator: validatorB, Balance: "7"}, {Validator: validatorB, Balance: "0", HoldCount: 1}},
		Redelegations: []Redelegation{{SourceValidator: validatorC, DestinationValidator: validatorA, Balance: "3"}}}
	// Execute actual child inserts with the deployed non-superuser permissions.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE structs_indexer`); err != nil {
		t.Fatal(err)
	}
	if ok, err := Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("same-height position upgrade: %v %v", ok, err)
	}
	assert(tx, true, 2, "18446744073709551616", "7", 2)
	// Invalid evidence fails before either parent or child state is changed.
	s.Bonded = "0"
	if ok, err := Publish(ctx, tx, s); err == nil || ok {
		t.Fatalf("partial snapshot published: %v %v", ok, err)
	}
	assert(tx, true, 2, "18446744073709551616", "7", 2)
	if _, err := tx.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=201 WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	request(t, ctx, tx, 201, Changes{Addresses: []string{testAddress}}, testAddress)
	assert(tx, false, 2, "18446744073709551616", "7", 2)
	s.Bonded = "18446744073709551616"
	if ok, err := Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("stale positions accepted: %v %v", ok, err)
	}
	// Accepted publication rolled back by its caller exposes neither zero parent
	// totals nor deletion of its old per-validator positions.
	empty := &Snapshot{ChainID: storeChain, Address: testAddress, Height: 201, Bonded: "0", Unbonding: "0"}
	child, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Rollback(context.Background()) })
	if ok, err := Publish(ctx, child, empty); err != nil || !ok {
		t.Fatalf("complete empty snapshot: %v %v", ok, err)
	}
	assert(child, true, 0, "0", "0", 0)
	if err := child.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assert(tx, false, 2, "18446744073709551616", "7", 2)
	if ok, err := Publish(ctx, tx, empty); err != nil || !ok {
		t.Fatalf("complete empty snapshot: %v %v", ok, err)
	}
	assert(tx, true, 0, "0", "0", 0)
}
