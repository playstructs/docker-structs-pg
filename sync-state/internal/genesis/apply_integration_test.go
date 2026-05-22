// Integration tests for genesis.Apply against a real Postgres. Opt-in
// via INTEGRATION_DATABASE_URL; each test cleans up its inserts so the
// dev DB stays usable.
//
// Why integration: the four-section insert logic + savepoint-per-section
// + replay-safe DELETE-on-reapply behaviour is the kind of thing that
// passes a unit test against a fake pgx.Tx but fails against a real DB
// for boring reasons (column types, constraint interactions, etc.).
// These tests run against the dev DB and clean up after themselves.

package genesis

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/db"
)

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func sampleLoaded(t *testing.T, chainID string) *LoadedDocument {
	t.Helper()
	raw := []byte(`{
		"genesis_time":"2026-01-01T00:00:00Z",
		"chain_id":"` + chainID + `",
		"app_state":{
			"bank":{"balances":[
				{"address":"addr_a","coins":[{"denom":"ualpha","amount":"100"},{"denom":"ore","amount":"5"}]},
				{"address":"addr_b","coins":[{"denom":"ualpha","amount":"200"}]}
			]},
			"staking":{
				"validators":[{"operator_address":"val_x","tokens":"1000","delegator_shares":"1000.000000000000000000"}],
				"delegations":[{"delegator_address":"addr_a","validator_address":"val_x","shares":"500.000000000000000000"}],
				"unbonding_delegations":[{"delegator_address":"addr_b","validator_address":"val_x","entries":[{"balance":"50"}]}]
			},
			"structs":{
				"playerList":[{"index":"1","primaryAddress":"addr_a"}],
				"gridList":[{"attributeId":"0-1-1","value":"42"}]
			}
		}
	}`)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &LoadedDocument{Doc: doc, Raw: raw, Source: "test:inline", SHA256: hashHex(raw)}
}

// cleanup wipes test rows so consecutive test runs don't pile up. We
// can't blanket-truncate structs.ledger (the live dev DB has real data
// from sync-state runs); instead match on the synthetic chain_id we put
// in genesis_log and the address/denom prefixes the fixture uses.
func cleanup(t *testing.T, pool *pgxpool.Pool, chainID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `DELETE FROM sync_state.genesis_log WHERE chain_id = $1`, chainID); err != nil {
		t.Logf("cleanup genesis_log: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM structs.ledger
		 WHERE action = 'genesis'
		   AND (address IN ('addr_a','addr_b','val_x') OR counterparty IN ('addr_a','addr_b','val_x'))
	`); err != nil {
		t.Logf("cleanup ledger: %v", err)
	}
}

func TestApply_RoundTrip(t *testing.T) {
	pool := openPool(t)
	chainID := "genesis-test-roundtrip"
	t.Cleanup(func() { cleanup(t, pool, chainID) })
	cleanup(t, pool, chainID)

	loaded := sampleLoaded(t, chainID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := Apply(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.LogRow.ChainID != chainID {
		t.Fatalf("logrow chain: %s", report.LogRow.ChainID)
	}
	// bank: 3 (addr_a/ualpha, addr_a/ore, addr_b/ualpha)
	// delegations: 2 (delegator + validator)
	// unbondings: 2 (delegator + validator)
	// ore: 1 (player addr_a, 0-1-1=42)
	want := map[string]int64{"bank": 3, "delegations": 2, "unbondings": 2, "ore": 1}
	for k, v := range want {
		if got := report.LogRow.RowsPerSection[k]; got != v {
			t.Errorf("section %s: got %d want %d (full=%+v)", k, got, v, report.LogRow.RowsPerSection)
		}
	}
	if report.LogRow.TotalRows != 8 {
		t.Errorf("total_rows: got %d want 8", report.LogRow.TotalRows)
	}

	// Spot-check the ledger landed.
	var balRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM structs.ledger
		 WHERE action='genesis' AND address='addr_a' AND denom='ualpha' AND amount_p='100'
	`).Scan(&balRows); err != nil {
		t.Fatalf("query bank: %v", err)
	}
	if balRows != 1 {
		t.Errorf("bank balance row count: %d want 1", balRows)
	}

	var delRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM structs.ledger
		 WHERE action='genesis' AND denom='ualpha.infused' AND amount_p='500'
		   AND ((address='addr_a' AND counterparty='val_x') OR (address='val_x' AND counterparty='addr_a'))
	`).Scan(&delRows); err != nil {
		t.Fatalf("query delegations: %v", err)
	}
	if delRows != 2 {
		t.Errorf("delegation rows: %d want 2", delRows)
	}

	var oreRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM structs.ledger
		 WHERE action='genesis' AND address='addr_a' AND denom='ore' AND amount_p='42'
	`).Scan(&oreRows); err != nil {
		t.Fatalf("query ore: %v", err)
	}
	if oreRows != 1 {
		t.Errorf("player ore rows: %d want 1", oreRows)
	}
}

// TestApply_Idempotent confirms that re-running Apply against the same
// document produces the same row counts and doesn't double-insert.
// This is the operational contract -force depends on.
func TestApply_Idempotent(t *testing.T) {
	pool := openPool(t)
	chainID := "genesis-test-idem"
	t.Cleanup(func() { cleanup(t, pool, chainID) })
	cleanup(t, pool, chainID)

	loaded := sampleLoaded(t, chainID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r1, err := Apply(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("apply #1: %v", err)
	}
	r2, err := Apply(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("apply #2: %v", err)
	}
	if r1.LogRow.TotalRows != r2.LogRow.TotalRows {
		t.Errorf("total_rows drift: r1=%d r2=%d", r1.LogRow.TotalRows, r2.LogRow.TotalRows)
	}
	if r2.PreDeleteRows != r1.LogRow.TotalRows {
		t.Errorf("second apply should have deleted exactly the first apply's rows; deleted=%d first_total=%d",
			r2.PreDeleteRows, r1.LogRow.TotalRows)
	}

	// Final state should match a single apply, not double it.
	var ledgerCount int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM structs.ledger
		 WHERE action='genesis' AND (address IN ('addr_a','addr_b','val_x') OR counterparty IN ('addr_a','addr_b','val_x'))
	`).Scan(&ledgerCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if ledgerCount != r1.LogRow.TotalRows {
		t.Errorf("ledger row count after second apply: got %d want %d", ledgerCount, r1.LogRow.TotalRows)
	}
}

// TestApply_LedgerInvariants confirms post-apply per-denom net sums
// match what the shell script's accounting model implies: bank ->
// 1 credit per coin entry; staking -> 2 credits per delegation (no
// debits to balance against — that's how the existing schema models
// staking positions, see Investigation 2 in the audit run).
func TestApply_LedgerInvariants(t *testing.T) {
	pool := openPool(t)
	chainID := "genesis-test-inv"
	t.Cleanup(func() { cleanup(t, pool, chainID) })
	cleanup(t, pool, chainID)

	loaded := sampleLoaded(t, chainID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Apply(ctx, pool, loaded); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cases := []struct {
		denom    string
		want     int64
	}{
		{"ualpha", 300},          // addr_a 100 + addr_b 200
		{"ore", 5 + 42},          // addr_a bank 5 + player ore 42
		{"ualpha.infused", 500 * 2}, // delegation -> 2 rows of 500
		{"ualpha.defusing", 50 * 2}, // unbonding   -> 2 rows of 50
	}
	for _, c := range cases {
		t.Run(c.denom, func(t *testing.T) {
			var sum int64
			err := pool.QueryRow(ctx, `
				SELECT COALESCE(SUM(amount_p)::bigint, 0)
				  FROM structs.ledger
				 WHERE action='genesis' AND denom=$1
				   AND (address IN ('addr_a','addr_b','val_x') OR counterparty IN ('addr_a','addr_b','val_x'))
			`, c.denom).Scan(&sum)
			if err != nil {
				t.Fatalf("sum: %v", err)
			}
			if sum != c.want {
				t.Errorf("denom %s sum: got %d want %d", c.denom, sum, c.want)
			}
		})
	}
}
