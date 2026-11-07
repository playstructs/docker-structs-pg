package sync

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/bank"
	"sync-state/internal/buffers"
	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/rpc"
)

// Exact public RPC responses, captured after v0.22.0 executed H. Keeping the
// original ordinals matters: legacy ledger identities refer to this stream.
//
//go:embed testdata/v022-upgrade-3353000-*.json.gz
var upgradeCapture embed.FS

func capturedUpgrade(t *testing.T) (*rpc.Client, *BlockBundle) {
	t.Helper()
	responses := make(map[string][]byte)
	for _, method := range []string{"block", "block_results"} {
		compressed, err := upgradeCapture.ReadFile("testdata/v022-upgrade-3353000-" + method + ".json.gz")
		if err != nil {
			t.Fatal(err)
		}
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			t.Fatal(err)
		}
		responses["/"+method], err = io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("height") != "3353000" || responses[r.URL.Path] == nil {
			http.Error(w, "fixture only serves the captured upgrade height", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responses[r.URL.Path])
	}))
	t.Cleanup(server.Close)
	client := rpc.NewClient([]string{server.URL}, 30*time.Second, 0)
	s := NewSyncer(Config{}, client, nil, events.NewRouter(false), "structstestnet-111")
	bundle, err := s.fetchBundle(context.Background(), 3353000)
	if err != nil {
		t.Fatal(err)
	}
	return client, bundle
}

func TestCapturedV022UpgradeWire(t *testing.T) {
	_, bundle := capturedUpgrade(t)
	if bundle.ChainID != "structstestnet-111" || bundle.Height != 3353000 || len(bundle.TxResults) != 0 {
		t.Fatalf("unexpected captured boundary: %+v", bundle)
	}
	counts := make(map[string]int)
	for _, event := range bundle.FinalizeBlockEvents {
		counts[event.Type]++
	}
	if counts["structs.structs.EventVaultAddress"] != 12327 || counts["structs.structs.EventOwnerReactorDiversity"] != 42 {
		t.Fatalf("captured upgrade envelopes were truncated: %v", counts)
	}
	buf := buffers.New()
	if err := bank.ProcessBlock(buf, bundle.Height, bundle.BlockTime, bundle.ChainID,
		bundle.FinalizeBlockEvents, bundle.TxResults); err != nil {
		t.Fatal(err)
	}
	if len(buf.Ledger) != 3300 {
		t.Fatalf("multi-denomination transfer legs = %d; want 3300", len(buf.Ledger))
	}
}

// This test needs the restored local H database, not a synthetic empty schema.
// The distinct opt-in URL and database comment guard prevent the ordinary
// integration suite from applying network fixtures to arbitrary test data.
// The caller also verifies the isolated Docker label before executing it.
func TestV022UpgradeRepresentativeReplay(t *testing.T) {
	url := os.Getenv("V022_UPGRADE_REHEARSAL_DATABASE_URL")
	if url == "" {
		t.Skip("V022_UPGRADE_REHEARSAL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var allowed bool
	if err := pool.QueryRow(ctx, `SELECT shobj_description(oid,'pg_database')='v022-representative-rehearsal'
FROM pg_database WHERE datname=current_database()`).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("refusing a database without the representative rehearsal marker: %v", err)
	}
	if err := db.CheckEconomySchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	client, bundle := capturedUpgrade(t)
	cursor, err := db.ReadCursor(ctx, pool, bundle.ChainID)
	if err != nil || cursor.LastHeight != bundle.Height || cursor.LastBlockHash != bundle.BlockHashHex {
		t.Fatalf("snapshot must be at exactly the captured upgrade boundary: %+v %v", cursor, err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	count := func(query string) int {
		t.Helper()
		var value int
		if err := tx.QueryRow(ctx, query).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if n := count(`SELECT count(*) FROM structs.vault`); n != 0 {
		t.Fatalf("expected the untouched old-writer snapshot with no vault rows, got %d", n)
	}
	if n := count(`SELECT count(*) FROM structs.ledger WHERE block_height=3353000`); n != 3254 {
		t.Fatalf("expected the captured legacy ledger's 3254 legs, got %d", n)
	}
	if n := count(`SELECT data_ready::int FROM structs.api_readiness(ARRAY['inventory'])`); n != 0 {
		t.Fatal("legacy writer state was reported ready before source reconciliation")
	}
	s := NewSyncer(Config{StatementTimeout: 2 * time.Minute, MirrorRaw: true}, client,
		&db.Pool{Pool: pool}, events.NewRouter(false), bundle.ChainID)
	for _, change := range []string{"height", "hash", "time"} {
		wrong := *bundle
		switch change {
		case "height":
			wrong.Height--
		case "hash":
			wrong.BlockHashHex = "different"
		case "time":
			wrong.BlockTime = wrong.BlockTime.Add(time.Second)
		}
		if _, err := s.applyBlockInTx(ctx, tx, &wrong, bundle.Height, buffers.New(), applyOpts{ReconcileCurrentBlock: true}); err == nil {
			t.Fatalf("accepted a different %s at the reconciliation boundary", change)
		}
	}
	// A conflicting legacy value must not be hidden by ON CONFLICT. This
	// savepoint leaves the restored source unchanged for the actual repair.
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := buffers.New()
	if err := bank.ProcessBlock(expected, bundle.Height, bundle.BlockTime, bundle.ChainID,
		bundle.FinalizeBlockEvents, bundle.TxResults); err != nil {
		t.Fatal(err)
	}
	r := expected.Ledger[0]
	changed, err := sp.Exec(ctx, `UPDATE structs.ledger SET amount_p=amount_p+1
WHERE time=$1 AND chain_id=$2 AND tx_index=$3 AND msg_index=$4 AND event_index=$5 AND denom=$6`,
		r.Time, r.ChainID, r.TxIndex, r.MsgIndex, r.EventIndex, r.Denom)
	if err != nil || changed.RowsAffected() != 1 {
		t.Fatalf("conflict fixture failed: %v %v", changed, err)
	}
	if err := expected.ValidateExistingLedger(ctx, sp); err == nil {
		t.Fatal("conflicting legacy amount was accepted")
	}
	if err := sp.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var first string
	for pass := 1; pass <= 2; pass++ {
		started := time.Now()
		result, err := s.applyBlockInTx(ctx, tx, bundle, bundle.Height, buffers.New(), applyOpts{ReconcileCurrentBlock: true})
		if err != nil {
			t.Fatalf("upgrade replay pass %d: %v", pass, err)
		}
		for _, diagnostic := range result.PendingErrors {
			if diagnostic.Severity == events.SeverityError {
				t.Fatalf("replay silently continued after an error: %+v", diagnostic)
			}
		}
		if n := count(`SELECT count(*) FROM structs.vault`); n != 12327 {
			t.Fatalf("vault rows = %d; want 12327", n)
		}
		if n := count(`SELECT count(*) FROM structs.ledger WHERE block_height=3353000`); n != 3300 {
			t.Fatalf("repaired ledger legs = %d; want 3300", n)
		}
		if n := count(`SELECT count(*) FROM structs.player_address pa JOIN structs.vault v ON v.address=pa.address`); n != 0 {
			t.Fatalf("vaults became signing addresses: %d", n)
		}
		if n := count(`SELECT count(*) FROM structs.vault WHERE account_created IS NULL`); n != 0 {
			t.Fatalf("vault origin evidence missing: %d", n)
		}
		if n := count(`SELECT count(*) FROM structs.owner_reactor_diversity`); n != 42 {
			t.Fatalf("owner diversity rows = %d; want 42", n)
		}
		if n := count(`SELECT data_ready::int FROM structs.api_readiness(ARRAY['inventory'])`); n != 1 {
			t.Fatal("reconciled source and projections were not reported ready")
		}
		for _, expected := range []struct {
			denom  string
			rows   int
			amount string
		}{
			{"ualpha", 1603, "188840538614"},
			{"uguild.0-1", 44, "1084649703249"},
			{"uguild.0-4", 1, "10000000"},
			{"uguild.0-5", 2, "2111842150000"},
		} {
			for _, direction := range []string{"credit", "debit"} {
				var rows int
				var amount string
				if err := tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(amount_p),0)::text FROM structs.ledger
WHERE block_height=3353000 AND denom=$1 AND direction=$2`, expected.denom, direction).Scan(&rows, &amount); err != nil {
					t.Fatal(err)
				}
				if rows != expected.rows || amount != expected.amount {
					t.Fatalf("%s %s rows=%d amount=%s; want %+v", expected.denom, direction, rows, amount, expected)
				}
			}
		}
		var snapshot string
		if err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
'ledger',(SELECT jsonb_agg(jsonb_build_array(denom,action,direction,rows,amount) ORDER BY denom,action,direction)
 FROM (SELECT denom,action,direction,count(*) rows,sum(amount_p)::text amount FROM structs.ledger
 WHERE block_height=3353000 GROUP BY denom,action,direction) totals),
'inventory',(SELECT md5(COALESCE(string_agg(md5(jsonb_build_array(owner_type,owner_id,denom,balance)::text),''
 ORDER BY owner_type,owner_id,denom),'')) FROM structs.api_inventory),
'diversity',(SELECT count(*) FROM structs.owner_reactor_diversity))::text`).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		if pass == 1 {
			first = snapshot
		} else if snapshot != first {
			t.Fatalf("repeated upgrade replay changed financial state:\n%s\n%s", first, snapshot)
		}
		t.Logf("pass %d: elapsed=%s financial_state=%s", pass, time.Since(started), snapshot)
	}
}
