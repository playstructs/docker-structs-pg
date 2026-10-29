package backfill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
	"sync-state/internal/readmodel"
)

func TestPlayersRefusePartialOrDuplicateSnapshot(t *testing.T) {
	for _, second := range []string{
		`{"Player":[{"id":"1-2"}],"pagination":{}}`,
		`{"Player":[{"id":"1-1","guildRank":"5"}],"pagination":{}}`,
		`{"Player":[{"id":"1-bad","guildRank":"5"}],"pagination":{}}`,
		`{"Player":[{"id":"1-2","guildRank":"18446744073709551616"}],"pagination":{}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Cosmos-Block-Height", "100")
			if r.URL.Query().Get("pagination.key") == "" {
				_, _ = w.Write([]byte(`{"Player":[{"id":"1-1","guildRank":"1"}],"pagination":{"next_key":"second"}}`))
			} else {
				_, _ = w.Write([]byte(second))
			}
		}))
		players, err := fetchAllPlayers(context.Background(), srv.Client(), srv.URL, 1, 100)
		srv.Close()
		if err == nil || players != nil {
			t.Fatalf("published partial snapshot: players=%v err=%v", players, err)
		}
	}
}

func TestPinnedPlanetSnapshotRangeAndBoundary(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := readmodel.Lock(ctx, tx); err != nil {
		t.Fatal(err)
	}
	boundary := snapshotBoundary{"snapshot-test", 100, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_state.sync_cursor(chain_id,last_height,last_block_time) VALUES($1,$2,$3)`, boundary.chainID, boundary.height, boundary.time); err != nil {
		t.Fatal(err)
	}
	if err := boundary.check(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state.sync_cursor SET last_height=101 WHERE chain_id=$1`, boundary.chainID); err != nil {
		t.Fatal(err)
	}
	if err := boundary.check(ctx, tx); err == nil {
		t.Fatal("accepted snapshot fetched before cursor advanced")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.planet_attribute(id,object_id,object_type,attribute_type,val)
VALUES('12-2-994199','2-994199','planet','blockStartOreMine',25)`); err != nil {
		t.Fatal(err)
	}
	rows, err := planetAttributeRows([]payload.PlanetAttribute{{AttributeID: "12-2-994198", Value: "18446744073709551615"}})
	if err != nil {
		t.Fatal(err)
	}
	var report PlanetAttributeReport
	// planet_attribute.val retains the pre-existing INTEGER storage contract.
	// An out-of-range snapshot must fail atomically, never skip its row and
	// then zero the previous value through the missing-row reconciliation.
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyPlanetAttributesInTx(ctx, sp, rows, &report); err == nil {
		t.Fatal("out-of-range planet attribute was coerced")
	}
	if err := sp.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var original string
	if err := tx.QueryRow(ctx, `SELECT val::text FROM structs.planet_attribute WHERE id='12-2-994199'`).Scan(&original); err != nil || original != "25" {
		t.Fatalf("failed snapshot changed existing state: value=%s err=%v", original, err)
	}
	rows[0].Val = 3353000
	if err := applyPlanetAttributesInTx(ctx, tx, rows, &report); err != nil {
		t.Fatal(err)
	}
	var exact, cleared string
	if err := tx.QueryRow(ctx, `SELECT val::text FROM structs.planet_attribute WHERE id='12-2-994198'`).Scan(&exact); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT val::text FROM structs.planet_attribute WHERE id='12-2-994199'`).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if exact != "3353000" || cleared != "0" {
		t.Fatalf("snapshot precision/clear: exact=%s cleared=%s", exact, cleared)
	}
	if needed, err := readmodel.NeedsBackfill(ctx, tx, 101); err != nil || !needed {
		t.Fatalf("changed snapshot left readiness published: needs_backfill=%v err=%v", needed, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.player(id,index,primary_address) VALUES('1-994199',994199,'snapshot-rank-address')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, updateGuildRankSQL, []string{"1-994199"}, []string{"18446744073709551615"}); err != nil {
		t.Fatal(err)
	}
	var rank string
	if err := tx.QueryRow(ctx, `SELECT guild_rank::text FROM structs.player WHERE id='1-994199'`).Scan(&rank); err != nil || rank != "18446744073709551615" {
		t.Fatalf("rank snapshot lost uint64 precision: rank=%s err=%v", rank, err)
	}
}
