// Integration tests for ProcessBlock against a real Postgres. Opt-in
// via INTEGRATION_DATABASE_URL; each test rolls back its transaction
// so the dev DB stays clean.
//
// Covers every event type ProcessBlock handles, including cross-event
// correlation (redelegate <- withdraw_rewards.delegator; create_validator
// <- coin_spent.spender) and the no-op behavior on missing/empty amount
// attributes.

package bank

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/rpc"
)

// processAndFlush wraps the Phase-2 buffer dance the production code
// does (sync/block.go and sync/bulkwindow.go): create a buffer, run
// ProcessBlock against it, then Flush via pgx.CopyFrom so the integration
// asserts that read from structs.ledger / structs.defusion still see
// the same rows the legacy per-INSERT path used to materialise.
func processAndFlush(ctx context.Context, tx pgx.Tx, height int64, blockTime time.Time, finalize []rpc.Event, txResults []rpc.TxResult) error {
	buf := buffers.New()
	if err := ProcessBlock(ctx, tx, buf, height, blockTime, finalize, txResults); err != nil {
		return err
	}
	return buf.Flush(ctx, tx)
}

func connect(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func inTx(t *testing.T, conn *pgx.Conn, body func(tx pgx.Tx)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	body(tx)
}

func fixedTime() time.Time {
	return time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
}

// transferEvent builds a finalize/tx event named "transfer".
func transferEvent(sender, recipient, amount string) rpc.Event {
	return rpc.Event{Type: "transfer", Attributes: []rpc.Attribute{
		{Key: "recipient", Value: recipient},
		{Key: "sender", Value: sender},
		{Key: "amount", Value: amount},
	}}
}

// queryLedgerCount returns the number of rows matching the predicates.
// Useful for asserting "wrote N rows" without spelling out every column.
func queryLedgerCount(t *testing.T, tx pgx.Tx, where string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM structs.ledger WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatalf("count ledger (%s): %v", where, err)
	}
	return n
}

// -------- transfer --------

func TestProcessBlock_Transfer_DebitAndCredit(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := transferEvent("structs1sender", "structs1recipient", "100ualpha")
		if err := processAndFlush(ctx, tx, 1000, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "address=$1 AND action='sent' AND direction='debit' AND denom='ualpha' AND block_height=1000", "structs1sender"); n != 1 {
			t.Errorf("sent debit rows = %d want 1", n)
		}
		if n := queryLedgerCount(t, tx, "address=$1 AND action='received' AND direction='credit' AND denom='ualpha' AND block_height=1000", "structs1recipient"); n != 1 {
			t.Errorf("received credit rows = %d want 1", n)
		}
		// Verify counterparty + amount + block_time on the debit row
		var counterparty string
		var amount int64
		var blockTime time.Time
		err := tx.QueryRow(ctx, `
			SELECT counterparty, amount_p::bigint, time
			  FROM structs.ledger
			 WHERE address='structs1sender' AND action='sent' AND block_height=1000`).
			Scan(&counterparty, &amount, &blockTime)
		if err != nil {
			t.Fatalf("scan debit: %v", err)
		}
		if counterparty != "structs1recipient" {
			t.Errorf("counterparty = %q", counterparty)
		}
		if amount != 100 {
			t.Errorf("amount = %d want 100", amount)
		}
		if !blockTime.Equal(fixedTime()) {
			t.Errorf("time = %s want %s", blockTime, fixedTime())
		}
	})
}

func TestProcessBlock_Transfer_EmptyAmountIsNoOp(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "transfer", Attributes: []rpc.Attribute{
			{Key: "recipient", Value: "structs1r"},
			{Key: "sender", Value: "structs1s"},
			{Key: "amount", Value: ""},
		}}
		if err := processAndFlush(ctx, tx, 1001, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "block_height=1001"); n != 0 {
			t.Errorf("expected no ledger rows for empty amount; got %d", n)
		}
	})
}

func TestProcessBlock_Transfer_MissingAmountIsNoOp(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "transfer", Attributes: []rpc.Attribute{
			{Key: "recipient", Value: "structs1r"},
			{Key: "sender", Value: "structs1s"},
		}}
		if err := processAndFlush(ctx, tx, 1002, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "block_height=1002"); n != 0 {
			t.Errorf("expected no ledger rows for missing amount; got %d", n)
		}
	})
}

// -------- coinbase --------

func TestProcessBlock_Coinbase(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "coinbase", Attributes: []rpc.Attribute{
			{Key: "minter", Value: "structs1minter"},
			{Key: "amount", Value: "500ualpha"},
		}}
		if err := processAndFlush(ctx, tx, 1010, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "address=$1 AND action='minted' AND direction='credit' AND denom='ualpha' AND block_height=1010", "structs1minter"); n != 1 {
			t.Errorf("minted rows = %d want 1", n)
		}
		var cp *string
		_ = tx.QueryRow(ctx, "SELECT counterparty FROM structs.ledger WHERE action='minted' AND block_height=1010").Scan(&cp)
		if cp != nil {
			t.Errorf("counterparty = %q want NULL", *cp)
		}
	})
}

// -------- burn --------

func TestProcessBlock_Burn(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "burn", Attributes: []rpc.Attribute{
			{Key: "burner", Value: "structs1burner"},
			{Key: "amount", Value: "777ualpha"},
		}}
		if err := processAndFlush(ctx, tx, 1020, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "address=$1 AND action='burned' AND direction='debit' AND denom='ualpha' AND block_height=1020", "structs1burner"); n != 1 {
			t.Errorf("burned rows = %d want 1", n)
		}
	})
}

// -------- delegate --------

func TestProcessBlock_Delegate_ThreeRowPattern(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "delegate", Attributes: []rpc.Attribute{
			{Key: "validator", Value: "structsvaloper1v"},
			{Key: "delegator", Value: "structs1d"},
			{Key: "amount", Value: "1000ualpha"},
		}}
		if err := processAndFlush(ctx, tx, 1030, fixedTime(), nil, []rpc.TxResult{{Events: []rpc.Event{ev}}}); err != nil {
			t.Fatalf("process: %v", err)
		}
		// 1: delegator debit ualpha
		if n := queryLedgerCount(t, tx, "address=$1 AND action='infused' AND direction='debit' AND denom='ualpha' AND block_height=1030", "structs1d"); n != 1 {
			t.Errorf("row 1 (delegator debit ualpha) = %d want 1", n)
		}
		// 2: delegator credit ualpha.infused
		if n := queryLedgerCount(t, tx, "address=$1 AND action='infused' AND direction='credit' AND denom='ualpha.infused' AND block_height=1030", "structs1d"); n != 1 {
			t.Errorf("row 2 (delegator credit ualpha.infused) = %d want 1", n)
		}
		// 3: validator credit ualpha.infused
		if n := queryLedgerCount(t, tx, "address=$1 AND action='infused' AND direction='credit' AND denom='ualpha.infused' AND block_height=1030", "structsvaloper1v"); n != 1 {
			t.Errorf("row 3 (validator credit ualpha.infused) = %d want 1", n)
		}
	})
}

// -------- redelegate (cross-event withdraw_rewards.delegator + defusion 'r') --------

func TestProcessBlock_Redelegate_CrossEventAndDefusion(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		completion := "2026-05-31T00:00:00Z"
		redel := rpc.Event{Type: "redelegate", Attributes: []rpc.Attribute{
			{Key: "source_validator", Value: "structsvaloper1src"},
			{Key: "destination_validator", Value: "structsvaloper1dst"},
			{Key: "amount", Value: "2500ualpha"},
			{Key: "completion_time", Value: completion},
		}}
		// Sibling event so cross-event lookup finds the delegator
		wr := rpc.Event{Type: "withdraw_rewards", Attributes: []rpc.Attribute{
			{Key: "delegator", Value: "structs1delegator"},
			{Key: "validator", Value: "structsvaloper1src"},
		}}
		if err := processAndFlush(ctx, tx, 1040, fixedTime(), nil, []rpc.TxResult{{Events: []rpc.Event{redel, wr}}}); err != nil {
			t.Fatalf("process: %v", err)
		}
		// 4 diversion_started rows
		if n := queryLedgerCount(t, tx, "action='diversion_started' AND block_height=1040"); n != 4 {
			t.Errorf("diversion_started rows = %d want 4", n)
		}
		// row 1: src_val debit .infused
		if n := queryLedgerCount(t, tx, "address=$1 AND direction='debit' AND denom='ualpha.infused' AND block_height=1040 AND action='diversion_started'", "structsvaloper1src"); n != 1 {
			t.Errorf("row 1 = %d want 1", n)
		}
		// row 3: dst_val credit .defusing
		if n := queryLedgerCount(t, tx, "address=$1 AND direction='credit' AND denom='ualpha.defusing' AND block_height=1040 AND action='diversion_started'", "structsvaloper1dst"); n != 1 {
			t.Errorf("row 3 = %d want 1", n)
		}

		// defusion row, type 'r'
		var typ, denom string
		var amount int64
		var compTime time.Time
		err := tx.QueryRow(ctx, `
			SELECT defusion_type, amount_p::bigint, denom, completed_at
			  FROM structs.defusion
			 WHERE validator_address=$1 AND delegator_address=$2
		`, "structsvaloper1dst", "structs1delegator").Scan(&typ, &amount, &denom, &compTime)
		if err != nil {
			t.Fatalf("scan defusion: %v", err)
		}
		if typ != "r" || denom != "ualpha" || amount != 2500 {
			t.Errorf("defusion: typ=%q denom=%q amt=%d", typ, denom, amount)
		}
		if !compTime.Equal(time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("completed_at = %s", compTime)
		}
	})
}

// -------- complete_redelegation --------

func TestProcessBlock_CompleteRedelegation(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "complete_redelegation", Attributes: []rpc.Attribute{
			{Key: "destination_validator", Value: "structsvaloper1dst"},
			{Key: "delegator", Value: "structs1d"},
			{Key: "amount", Value: "750ualpha"},
		}}
		if err := processAndFlush(ctx, tx, 1050, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "action='diversion_completed' AND block_height=1050"); n != 4 {
			t.Errorf("diversion_completed rows = %d want 4", n)
		}
		if n := queryLedgerCount(t, tx, "action='diversion_completed' AND direction='debit' AND denom='ualpha.defusing' AND block_height=1050"); n != 2 {
			t.Errorf("defusing debits = %d want 2", n)
		}
		if n := queryLedgerCount(t, tx, "action='diversion_completed' AND direction='credit' AND denom='ualpha.infused' AND block_height=1050"); n != 2 {
			t.Errorf("infused credits = %d want 2", n)
		}
	})
}

// -------- unbond + defusion 'u' --------

func TestProcessBlock_Unbond_FourRowsPlusDefusion(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		completion := "2026-06-15T12:00:00Z"
		ev := rpc.Event{Type: "unbond", Attributes: []rpc.Attribute{
			{Key: "validator", Value: "structsvaloper1u"},
			{Key: "delegator", Value: "structs1u"},
			{Key: "amount", Value: "300ualpha"},
			{Key: "completion_time", Value: completion},
		}}
		if err := processAndFlush(ctx, tx, 1060, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "action='defusion_started' AND block_height=1060"); n != 4 {
			t.Errorf("defusion_started rows = %d want 4", n)
		}
		var typ string
		err := tx.QueryRow(ctx, `
			SELECT defusion_type FROM structs.defusion
			 WHERE validator_address='structsvaloper1u' AND delegator_address='structs1u'`).Scan(&typ)
		if err != nil {
			t.Fatalf("scan defusion: %v", err)
		}
		if typ != "u" {
			t.Errorf("defusion type = %q want u", typ)
		}
	})
}

// -------- cancel_unbond --------

func TestProcessBlock_CancelUnbond(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "cancel_unbond", Attributes: []rpc.Attribute{
			{Key: "validator", Value: "structsvaloper1c"},
			{Key: "delegator", Value: "structs1c"},
			{Key: "amount", Value: "120ualpha"},
		}}
		if err := processAndFlush(ctx, tx, 1070, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "action='defusion_cancelled' AND block_height=1070"); n != 4 {
			t.Errorf("defusion_cancelled rows = %d want 4", n)
		}
		// No defusion row for cancel
		var cnt int
		_ = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM structs.defusion
			 WHERE delegator_address='structs1c'`).Scan(&cnt)
		if cnt != 0 {
			t.Errorf("defusion rows for cancel_unbond = %d want 0", cnt)
		}
	})
}

// -------- complete_unbonding --------

func TestProcessBlock_CompleteUnbonding_ReturnsBaseDenom(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		ev := rpc.Event{Type: "complete_unbonding", Attributes: []rpc.Attribute{
			{Key: "validator", Value: "structsvaloper1f"},
			{Key: "delegator", Value: "structs1f"},
			{Key: "amount", Value: "999ualpha"},
		}}
		if err := processAndFlush(ctx, tx, 1080, fixedTime(), []rpc.Event{ev}, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		// 3 rows total: 2 defusing debits + 1 base-denom credit
		if n := queryLedgerCount(t, tx, "action='defusion_completed' AND block_height=1080"); n != 3 {
			t.Errorf("defusion_completed rows = %d want 3", n)
		}
		if n := queryLedgerCount(t, tx, "action='defusion_completed' AND direction='credit' AND denom='ualpha' AND block_height=1080"); n != 1 {
			t.Errorf("base-denom credit rows = %d want 1", n)
		}
		if n := queryLedgerCount(t, tx, "action='defusion_completed' AND direction='debit' AND denom='ualpha.defusing' AND block_height=1080"); n != 2 {
			t.Errorf("defusing debit rows = %d want 2", n)
		}
	})
}

// -------- create_validator (cross-event coin_spent.spender) --------

func TestProcessBlock_CreateValidator_CrossEventSpender(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		cv := rpc.Event{Type: "create_validator", Attributes: []rpc.Attribute{
			{Key: "validator", Value: "structsvaloper1new"},
			{Key: "amount", Value: "5000ualpha"},
		}}
		spend := rpc.Event{Type: "coin_spent", Attributes: []rpc.Attribute{
			{Key: "spender", Value: "structs1selfdelegate"},
			{Key: "amount", Value: "5000ualpha"},
		}}
		if err := processAndFlush(ctx, tx, 1090, fixedTime(), nil, []rpc.TxResult{{Events: []rpc.Event{cv, spend}}}); err != nil {
			t.Fatalf("process: %v", err)
		}
		if n := queryLedgerCount(t, tx, "action='infused' AND block_height=1090"); n != 3 {
			t.Errorf("infused rows = %d want 3", n)
		}
		// row 1: spender debit base denom
		if n := queryLedgerCount(t, tx, "address=$1 AND direction='debit' AND denom='ualpha' AND block_height=1090 AND action='infused'", "structs1selfdelegate"); n != 1 {
			t.Errorf("spender debit row = %d want 1", n)
		}
		// row 2: validator credit .infused, row 3: spender credit .infused
		if n := queryLedgerCount(t, tx, "direction='credit' AND denom='ualpha.infused' AND block_height=1090 AND action='infused'"); n != 2 {
			t.Errorf("infused credits = %d want 2", n)
		}
	})
}

// -------- isolation: bank failure in one event doesn't poison the rest --------

func TestProcessBlock_NonBankEventsAreSkipped(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		evs := []rpc.Event{
			{Type: "structs.structs.EventPlayer", Attributes: []rpc.Attribute{{Key: "player", Value: "{}"}}},
			{Type: "tx", Attributes: nil},
			transferEvent("structs1s", "structs1r", "1ualpha"),
		}
		if err := processAndFlush(ctx, tx, 1100, fixedTime(), evs, nil); err != nil {
			t.Fatalf("process: %v", err)
		}
		// Only the transfer should have produced 2 rows
		if n := queryLedgerCount(t, tx, "block_height=1100"); n != 2 {
			t.Errorf("ledger rows at 1100 = %d want 2", n)
		}
	})
}
