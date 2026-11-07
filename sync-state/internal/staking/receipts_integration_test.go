package staking_test

import (
	"context"
	"os"
	"sync-state/internal/staking"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/bank"
	"sync-state/internal/buffers"
	"sync-state/internal/rpc"
)

const storeChain = "native-receipts-integration"
const testAddress = "structs1q3nx9j4qcqaf4p2qjc9zxqxrccg4renc2fyaw5"
const validatorA = "structsvaloper1pa4ch99mktue309avws5xxp70t4rr5egxfmcmp"
const validatorB = "structsvaloper1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
const validatorC = "structsvaloper1rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"

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
VALUES($1,200,'2026-10-29T12:00:00Z','current')`, storeChain); err != nil {
		t.Fatal(err)
	}
	return ctx, tx
}

func request(t *testing.T, ctx context.Context, tx pgx.Tx, height int64, changes staking.Changes, want string) {
	t.Helper()
	got, err := staking.Request(ctx, tx, storeChain, height, changes)
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("request %v %v", got, err)
	}
}

func receiptEvents(address, validator string, amount string, completion time.Time) rpc.Event {
	return rpc.Event{Type: "unbond", Attributes: []rpc.Attribute{
		{Key: "delegator", Value: address}, {Key: "validator", Value: validator},
		{Key: "amount", Value: amount}, {Key: "completion_time", Value: completion.Format(time.RFC3339)},
	}}
}

func writeReceipts(t *testing.T, ctx context.Context, tx pgx.Tx, height int64, events []rpc.Event) {
	t.Helper()
	b := buffers.New()
	if err := bank.ProcessBlock(b, height, time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), storeChain, nil, []rpc.TxResult{{Events: events}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Flush(ctx, tx); err != nil {
		t.Fatal(err)
	}
}

func TestReceipts_ReplayHeldMergedGroupsAndAuthoritativeAbsence(t *testing.T) {
	ctx, tx := storeTx(t)
	// Run the actual writer and publication as the non-superuser service role.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE structs_indexer`); err != nil {
		t.Fatal(err)
	}
	completion := time.Date(2026, 5, 1, 0, 0, 0, 123456789, time.UTC) // overdue is still held
	events := []rpc.Event{
		receiptEvents(testAddress, validatorA, "18446744073709551616ualpha", completion),
		receiptEvents(testAddress, validatorA, "2ualpha", completion),
		receiptEvents(testAddress, validatorB, "3ualpha", completion),
	}
	writeReceipts(t, ctx, tx, 100, events)
	writeReceipts(t, ctx, tx, 100, events)
	check := func(total, active int, amount string) {
		t.Helper()
		var n, a int
		var v string
		if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE retired_height IS NULL),sum(amount_p)::text
FROM structs.defusion WHERE chain_id=$1`, storeChain).Scan(&n, &a, &v); err != nil {
			t.Fatal(err)
		}
		if n != total || a != active || v != amount {
			t.Fatalf("receipts total=%d active=%d amount=%s", n, a, v)
		}
	}
	check(3, 3, "18446744073709551621")
	// The merged/slashed/partly cancelled A entry remains; zero held B remains.
	// No initial receipt amounts are rewritten to masquerade as current balances.
	s := &staking.Snapshot{ChainID: storeChain, Address: testAddress, Height: 150, Bonded: "0", Unbonding: "4",
		Unbondings: []staking.Unbonding{
			{Validator: validatorA, CreationHeight: 100, CompletionTime: completion, Balance: "4", HoldCount: 1},
			{Validator: validatorB, CreationHeight: 100, CompletionTime: completion, Balance: "0", HoldCount: 1},
		}}
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
	check(3, 3, "18446744073709551621")
	if _, err := tx.Exec(ctx, `CALL structs.CLEAN_DEFUSION()`); err != nil {
		t.Fatal(err)
	}
	check(3, 3, "18446744073709551621")
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.defusion_current WHERE chain_id=$1 AND pending_state='pending'`, storeChain).Scan(&pending); err != nil || pending != 3 {
		t.Fatalf("pending=%d %v", pending, err)
	}
	request(t, ctx, tx, 151, staking.Changes{Addresses: []string{testAddress}}, testAddress)
	if ok, err := staking.Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("stale publication %v %v", ok, err)
	}
	// B disappears after a zero-payment completion; A is still held.
	s.Height = 151
	s.Unbondings = s.Unbondings[:1]
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
	check(3, 2, "18446744073709551621")
	writeReceipts(t, ctx, tx, 100, events)
	check(3, 2, "18446744073709551621")
	// New historical recovery (a previously missing same-block receipt) queues
	// a same-height refresh without making valid balance evidence unknown.
	events = append(events, receiptEvents(testAddress, validatorB, "7ualpha", completion))
	writeReceipts(t, ctx, tx, 100, events)
	var queued, complete bool
	if err := tx.QueryRow(ctx, `SELECT s.receipts_pending,c.complete FROM sync_state.native_staking_snapshot s
JOIN sync_state.native_staking_claim c USING(chain_id,address) WHERE s.chain_id=$1 AND s.address=$2`, storeChain, testAddress).Scan(&queued, &complete); err != nil || !queued || !complete {
		t.Fatalf("queued=%v complete=%v %v", queued, complete, err)
	}
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("same-height receipt recovery %v %v", ok, err)
	}
	check(4, 2, "18446744073709551628")
	// Current complete-empty evidence retires the group exactly once.
	request(t, ctx, tx, 152, staking.Changes{Addresses: []string{testAddress}}, testAddress)
	s.Height = 152
	s.Unbondings = nil
	s.Unbonding = "0"
	if _, err := tx.Exec(ctx, `SAVEPOINT receipt_publication`); err != nil {
		t.Fatal(err)
	}
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("empty publication %v %v", ok, err)
	}
	check(4, 0, "18446744073709551628")
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT receipt_publication`); err != nil {
		t.Fatal(err)
	}
	check(4, 2, "18446744073709551628")
	var sourceHeight int64
	var incomplete bool
	if err := tx.QueryRow(ctx, `SELECT source_height,pending FROM sync_state.native_staking_snapshot
WHERE chain_id=$1 AND address=$2`, storeChain, testAddress).Scan(&sourceHeight, &incomplete); err != nil || sourceHeight != 151 || !incomplete {
		t.Fatalf("partial publication survived rollback %d %v %v", sourceHeight, incomplete, err)
	}
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("retry after rollback %v %v", ok, err)
	}
	writeReceipts(t, ctx, tx, 100, events)
	check(4, 0, "18446744073709551628")
	if err := tx.QueryRow(ctx, `SELECT receipts_pending FROM sync_state.native_staking_snapshot WHERE chain_id=$1 AND address=$2`, storeChain, testAddress).Scan(&queued); err != nil || queued {
		t.Fatalf("duplicate replay queued new work: %v %v", queued, err)
	}
}

func TestReceipts_RedelegationHeightAndLegacyBounds(t *testing.T) {
	ctx, tx := storeTx(t)
	// A sole chain is required to adopt unidentified pre-migration receipts.
	if _, err := tx.Exec(ctx, `DELETE FROM sync_state.sync_cursor WHERE chain_id<>$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	completion := time.Date(2026, 5, 1, 0, 0, 0, 987654321, time.UTC)
	events := []rpc.Event{
		{Type: "withdraw_rewards", Attributes: []rpc.Attribute{{Key: "delegator", Value: testAddress}, {Key: "validator", Value: validatorA}}},
		{Type: "redelegate", Attributes: []rpc.Attribute{{Key: "source_validator", Value: validatorA}, {Key: "destination_validator", Value: validatorB}, {Key: "amount", Value: "5ualpha"}, {Key: "completion_time", Value: completion.Format(time.RFC3339)}}},
	}
	writeReceipts(t, ctx, tx, 100, events)
	if _, err := tx.Exec(ctx, `INSERT INTO structs.defusion(validator_address,delegator_address,defusion_type,amount_p,denom,completed_at)
SELECT $1,$2,'r',5,'ualpha',$3 FROM generate_series(1,2)`, validatorB, testAddress, completion.Truncate(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := staking.SeedReceipts(ctx, tx, storeChain, 200); err != nil {
		t.Fatal(err)
	}
	// Source validator began unbonding before this redelegation. Do not compare
	// its native creation_height=80 to the receipt's transaction height=100.
	s := &staking.Snapshot{ChainID: storeChain, Address: testAddress, Height: 199, Bonded: "5", Unbonding: "0",
		Delegations:   []staking.Delegation{{Validator: validatorB, Balance: "5"}},
		Redelegations: []staking.Redelegation{{SourceValidator: validatorA, DestinationValidator: validatorB, CreationHeight: 80, CompletionTime: completion, Balance: "5"}}}
	if ok, err := staking.Publish(ctx, tx, s); err != nil || ok {
		t.Fatalf("legacy upper bound ignored %v %v", ok, err)
	}
	s.Height = 200
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
	var active, unidentified int
	if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE pending_state='unidentified') FROM structs.defusion_current WHERE chain_id=$1`, storeChain).Scan(&active, &unidentified); err != nil || active != 3 || unidentified != 2 {
		t.Fatalf("active=%d unidentified=%d %v", active, unidentified, err)
	}
	// Absence is about endpoints too: an unrelated source with the same
	// destination/date may conservatively retain legacy rows, not this receipt.
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=201 WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	s.Height = 201
	s.Redelegations[0].SourceValidator = validatorC
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.defusion_current WHERE chain_id=$1`, storeChain).Scan(&active); err != nil || active != 2 {
		t.Fatalf("wrong source retained identified receipt %d %v", active, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=202 WHERE chain_id=$1`, storeChain); err != nil {
		t.Fatal(err)
	}
	s.Height = 202
	s.Redelegations = nil
	if ok, err := staking.Publish(ctx, tx, s); err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.defusion_current WHERE chain_id=$1`, storeChain).Scan(&active); err != nil || active != 0 {
		t.Fatalf("legacy not reconciled %d %v", active, err)
	}
	// Source coordinates remain absent on both preserved legacy tombstones.
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.defusion WHERE chain_id=$1 AND block_height IS NULL AND tx_index IS NULL AND event_index IS NULL AND legacy_observed_height=200 AND retired_height=202`, storeChain).Scan(&unidentified); err != nil || unidentified != 2 {
		t.Fatalf("legacy provenance lost %d %v", unidentified, err)
	}
}
