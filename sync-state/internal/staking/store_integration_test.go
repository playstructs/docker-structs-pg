package staking

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/readmodel"
)

const storeChain = "native-snapshot-integration"
const storeAddressB = "structs1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
const storeAddressC = "structs1rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"

func TestInvalidateNoOwnerIDsSkipsDatabase(t *testing.T) {
	dirty := readmodel.NewDirty()
	dirty.Provider("4-1")
	if err := Invalidate(t.Context(), nil, storeChain, 100, dirty, Changes{}); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidateValidatorWithoutOwners(t *testing.T) {
	ctx, tx := storeTx(t)
	if err := Invalidate(ctx, tx, storeChain, 100, readmodel.NewDirty(), Changes{Validators: []string{validatorA}}); err != nil {
		t.Fatal(err)
	}
	var height int64
	if err := tx.QueryRow(ctx, `SELECT height FROM sync_state.native_staking_invalidation WHERE chain_id=$1 AND validator=$2`,
		storeChain, validatorA).Scan(&height); err != nil || height != 100 {
		t.Fatalf("validator invalidation: height=%d err=%v", height, err)
	}
}

func TestInvalidateDiscoversOwners(t *testing.T) {
	ctx, tx := storeTx(t)
	const player, guild = "1-997101", "0-997101"
	if _, err := tx.Exec(ctx, `INSERT INTO structs.guild(id,index,name) VALUES ($1,0,'staking test')`, guild); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.player(id,primary_address) VALUES ($1,$2)`, player, testAddress); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.vault(owner_id,address,account_number) VALUES ($1,$2,1),($3,$4,2)`,
		player, storeAddressB, guild, storeAddressC); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mark func(*readmodel.Dirty, string)
		id   string
		want string
	}{
		{"address", (*readmodel.Dirty).Address, storeAddressB, storeAddressB},
		{"player", (*readmodel.Dirty).Player, player, storeAddressB},
		{"guild", (*readmodel.Dirty).Guild, guild, storeAddressC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirty := readmodel.NewDirty()
			tc.mark(dirty, tc.id)
			chain := storeChain + "-" + tc.name
			if err := Invalidate(ctx, tx, chain, 100, dirty, Changes{}); err != nil {
				t.Fatal(err)
			}
			var height int64
			if err := tx.QueryRow(ctx, `SELECT required_height FROM sync_state.native_staking_snapshot WHERE chain_id=$1 AND address=$2`, chain, tc.want).Scan(&height); err != nil || height != 100 {
				t.Fatalf("owner %s: height=%d err=%v", tc.want, height, err)
			}
		})
	}
}

func storeTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `INSERT INTO sync_state.sync_cursor(chain_id,last_height,last_block_time,status)
VALUES ($1,200,'2026-10-29T12:00:00Z','current')`, storeChain); err != nil {
		t.Fatal(err)
	}
	return ctx, tx
}

func request(t *testing.T, ctx context.Context, tx pgx.Tx, height int64, changes Changes, want ...string) {
	t.Helper()
	got, err := Request(ctx, tx, storeChain, height, changes)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) || (len(got) != 0 && !reflect.DeepEqual(got, want)) {
		t.Fatalf("invalidated %v; want %v", got, want)
	}
}

func storedClaim(t *testing.T, ctx context.Context, tx pgx.Tx, address string, complete bool, bonded, unbonding *string) {
	t.Helper()
	var actualComplete bool
	var actualBonded, actualUnbonding *string
	if err := tx.QueryRow(ctx, `SELECT complete,bonded::text,unbonding::text
FROM sync_state.native_staking_claim WHERE chain_id=$1 AND address=$2`, storeChain, address).
		Scan(&actualComplete, &actualBonded, &actualUnbonding); err != nil {
		t.Fatal(err)
	}
	if actualComplete != complete || !reflect.DeepEqual(actualBonded, bonded) || !reflect.DeepEqual(actualUnbonding, unbonding) {
		t.Fatalf("claim complete=%v bonded=%v unbonding=%v; want %v %v %v", actualComplete, actualBonded, actualUnbonding, complete, bonded, unbonding)
	}
}

func TestStore_StaleResultsNeverPublishAndCompletionStaysZero(t *testing.T) {
	ctx, tx := storeTx(t)
	request(t, ctx, tx, 100, Changes{Addresses: []string{testAddress, testAddress}}, testAddress)
	storedClaim(t, ctx, tx, testAddress, false, nil, nil)
	large, pending, zero := "18446744073709551616", "340", "0"
	s := &Snapshot{ChainID: storeChain, Address: testAddress, Height: 100, Bonded: large, Unbonding: pending,
		Delegations: []Delegation{{Validator: validatorA, Balance: large}}, Unbondings: []Unbonding{{Validator: validatorB, Balance: pending}}}
	if ok, err := Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	storedClaim(t, ctx, tx, testAddress, true, &large, &pending)
	// An ordinary transfer/ownership lookup must not continually invalidate it.
	if err := Ensure(ctx, tx, storeChain, 150, []string{testAddress}); err != nil {
		t.Fatal(err)
	}
	request(t, ctx, tx, 90, Changes{Addresses: []string{testAddress}})
	storedClaim(t, ctx, tx, testAddress, true, &large, &pending)
	request(t, ctx, tx, 101, Changes{Addresses: []string{testAddress}}, testAddress)
	storedClaim(t, ctx, tx, testAddress, false, nil, nil)
	if ok, err := Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("stale result accepted: %v %v", ok, err)
	}
	// Native completion removes pending now; a delayed Structs snapshot merely
	// requests another query, and cannot copy its stale defusing into this cache.
	s.Height, s.Unbonding, s.Unbondings = 102, zero, nil
	if ok, err := Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("completion: %v %v", ok, err)
	}
	storedClaim(t, ctx, tx, testAddress, true, &large, &zero)
	request(t, ctx, tx, 103, Changes{Addresses: []string{testAddress}}, testAddress)
	s.Height = 103
	if ok, err := Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("cleanup reconciliation: %v %v", ok, err)
	}
	storedClaim(t, ctx, tx, testAddress, true, &large, &zero)
	var exposure int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sync_state.native_staking_exposure WHERE chain_id=$1 AND address=$2`, storeChain, testAddress).Scan(&exposure); err != nil || exposure != 1 {
		t.Fatalf("obsolete pending exposure remained: %d %v", exposure, err)
	}
}

func TestStore_ValidatorExposureIncludesRedelegationSourceAndExcludesEmptyClaims(t *testing.T) {
	ctx, tx := storeTx(t)
	if err := Ensure(ctx, tx, storeChain, 100, []string{testAddress, storeAddressB, storeAddressC}); err != nil {
		t.Fatal(err)
	}
	snapshots := []*Snapshot{
		{ChainID: storeChain, Address: testAddress, Height: 100, Bonded: "20", Unbonding: "0",
			Delegations: []Delegation{{Validator: validatorA, Balance: "20"}}},
		{ChainID: storeChain, Address: storeAddressB, Height: 100, Bonded: "30", Unbonding: "0",
			Delegations:   []Delegation{{Validator: validatorB, Balance: "30"}},
			Redelegations: []Redelegation{{SourceValidator: validatorA, DestinationValidator: validatorB, Balance: "30"}}},
		{ChainID: storeChain, Address: storeAddressC, Height: 100, Bonded: "0", Unbonding: "0"},
	}
	for _, s := range snapshots {
		if ok, err := Publish(ctx, tx, s); err != nil || !ok {
			t.Fatalf("publish: %v %v", ok, err)
		}
	}
	request(t, ctx, tx, 101, Changes{Validators: []string{validatorA, validatorA}}, testAddress, storeAddressB)
	storedClaim(t, ctx, tx, testAddress, false, nil, nil)
	storedClaim(t, ctx, tx, storeAddressB, false, nil, nil)
	zero := "0"
	storedClaim(t, ctx, tx, storeAddressC, true, &zero, &zero)
	request(t, ctx, tx, 102, Changes{AllExposed: true}, testAddress, storeAddressB)
	// An unrelated chain's slash cannot affect these snapshots.
	if got, err := Request(ctx, tx, "other-chain", 199, Changes{AllExposed: true}); err != nil || len(got) != 0 {
		t.Fatalf("cross-chain invalidation: %v %v", got, err)
	}
}

func TestStore_RetryBudgetAndPublicationBoundary(t *testing.T) {
	ctx, tx := storeTx(t)
	request(t, ctx, tx, 100, Changes{Addresses: []string{testAddress, storeAddressB, storeAddressC}}, testAddress, storeAddressB, storeAddressC)
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET status='catching_up' WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	if items, err := Pending(ctx, tx, storeChain, 3); err != nil || len(items) != 0 {
		t.Fatalf("bulk catch-up scheduled historical HTTP work: %v %v", items, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET status='current' WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	if err := RetryLater(ctx, tx, storeChain, testAddress, 100, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := Pending(ctx, tx, storeChain, 1)
	if err != nil || len(items) != 1 || items[0].Address == testAddress || items[0].Height != 200 {
		t.Fatalf("bounded ready batch: %v %v", items, err)
	}
	request(t, ctx, tx, 101, Changes{Addresses: []string{testAddress}}, testAddress)
	// A failed older request must not delay work for the new invalidation.
	if err := RetryLater(ctx, tx, storeChain, testAddress, 100, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err = Pending(ctx, tx, storeChain, 3)
	if err != nil || len(items) != 3 {
		t.Fatalf("new invalidation delayed: %v %v", items, err)
	}
	s := &Snapshot{ChainID: storeChain, Address: testAddress, Height: 201, Bonded: "0", Unbonding: "0"}
	if ok, err := Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("future result: %v %v", ok, err)
	}
	s.Height, s.ChainID = 200, "other-chain"
	if ok, err := Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("wrong-chain result: %v %v", ok, err)
	}
	s.ChainID = storeChain
	if ok, err := Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("current result: %v %v", ok, err)
	}
	// Idempotent publication does not replace exposure or mutate totals again.
	if ok, err := Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("duplicate publication: %v %v", ok, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=199 WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	storedClaim(t, ctx, tx, testAddress, false, nil, nil)
}

func TestStore_UnseenExposureStillRejectsOldSnapshot(t *testing.T) {
	ctx, tx := storeTx(t)
	request(t, ctx, tx, 100, Changes{Addresses: []string{testAddress}}, testAddress)
	// No exposure is indexed yet. A source-validator change must still be
	// remembered while the first snapshot discovers an in-flight redelegation.
	request(t, ctx, tx, 101, Changes{Validators: []string{validatorC}})
	s := &Snapshot{ChainID: storeChain, Address: testAddress, Height: 100, Bonded: "30", Unbonding: "0",
		Delegations:   []Delegation{{Validator: validatorB, Balance: "30"}},
		Redelegations: []Redelegation{{SourceValidator: validatorC, DestinationValidator: validatorB, Balance: "30"}}}
	if ok, err := Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("newly discovered source exposure accepted stale evidence: %v %v", ok, err)
	}
	storedClaim(t, ctx, tx, testAddress, false, nil, nil)
	request(t, ctx, tx, 102, Changes{AllExposed: true})
	s.Height = 101
	if ok, err := Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("unmapped slash accepted newly discovered stale evidence: %v %v", ok, err)
	}
	s.Height = 102
	if ok, err := Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("current evidence refused: %v %v", ok, err)
	}
	// Replay cannot lower either watermark.
	request(t, ctx, tx, 90, Changes{Validators: []string{validatorC}, AllExposed: true})
	var minimum, maximum int64
	if err := tx.QueryRow(ctx, `SELECT MIN(height),MAX(height) FROM sync_state.native_staking_invalidation
WHERE chain_id=$1`, storeChain).Scan(&minimum, &maximum); err != nil || minimum != 101 || maximum != 102 {
		t.Fatalf("invalidation watermark regressed: %d %d %v", minimum, maximum, err)
	}
}
