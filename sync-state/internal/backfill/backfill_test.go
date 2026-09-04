package backfill

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/payload"
)

func TestFetchAllPlayers_TwoPages(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/structs/player" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("pagination.limit") != "2" {
			t.Errorf("pagination.limit = %q want 2", q.Get("pagination.limit"))
		}
		key := q.Get("pagination.key")
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "":
			next := "page2"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Player": []map[string]any{
					{"id": "1-1", "guildRank": "1"},
					{"id": "1-2", "guildRank": "2"},
				},
				"pagination": map[string]any{"next_key": next},
			})
		case "page2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Player": []map[string]any{
					{"id": "1-3", "guildRank": "101"},
				},
				"pagination": map[string]any{"next_key": nil},
			})
		default:
			t.Errorf("unexpected pagination.key %q", key)
			http.Error(w, "bad key", 400)
		}
	}))
	defer srv.Close()

	players, err := fetchAllPlayers(context.Background(), srv.Client(), srv.URL, 2)
	if err != nil {
		t.Fatalf("fetchAllPlayers: %v", err)
	}
	if hits != 2 {
		t.Errorf("HTTP hits = %d want 2", hits)
	}
	if len(players) != 3 {
		t.Fatalf("len(players) = %d want 3", len(players))
	}
	want := []struct {
		id   string
		rank int64
	}{
		{"1-1", 1},
		{"1-2", 2},
		{"1-3", 101},
	}
	for i, w := range want {
		if players[i].ID != w.id {
			t.Errorf("players[%d].ID = %q want %q", i, players[i].ID, w.id)
		}
		if players[i].GuildRank.Int64() != w.rank {
			t.Errorf("players[%d].GuildRank = %d want %d", i, players[i].GuildRank.Int64(), w.rank)
		}
	}
}

func TestFetchAllPlayers_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Player":     []any{},
			"pagination": map[string]any{},
		})
	}))
	defer srv.Close()

	players, err := fetchAllPlayers(context.Background(), srv.Client(), srv.URL, 100)
	if err != nil {
		t.Fatalf("fetchAllPlayers: %v", err)
	}
	if len(players) != 0 {
		t.Errorf("len = %d want 0", len(players))
	}
}

func TestFetchAllPlayers_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := fetchAllPlayers(context.Background(), srv.Client(), srv.URL, 10)
	if err == nil {
		t.Fatal("expected error on HTTP 502")
	}
}

func TestFetchAllPlayers_EscapesPaginationKey(t *testing.T) {
	// Ensure pagination.key is URL-escaped (base64 keys can contain +/=).
	rawKey := "MS0xMDA="
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("pagination.key")
		if gotKey == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Player":     []map[string]any{{"id": "1-1", "guildRank": "1"}},
				"pagination": map[string]any{"next_key": rawKey},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Player":     []map[string]any{{"id": "1-2", "guildRank": "2"}},
			"pagination": map[string]any{},
		})
	}))
	defer srv.Close()

	players, err := fetchAllPlayers(context.Background(), srv.Client(), srv.URL+"/", 1)
	if err != nil {
		t.Fatalf("fetchAllPlayers: %v", err)
	}
	if len(players) != 2 {
		t.Fatalf("len = %d want 2", len(players))
	}
	if gotKey != rawKey {
		t.Errorf("pagination.key = %q want %q (escaped round-trip)", gotKey, rawKey)
	}
	// Sanity: url.QueryEscape of the key should decode back.
	if u, err := url.QueryUnescape(url.QueryEscape(rawKey)); err != nil || u != rawKey {
		t.Errorf("escape round-trip failed: %v %q", err, u)
	}
}

// TestApplyDefenderPlanetary_CorrectsAndIdempotent verifies the sweep SQL
// flips a deliberately wrong is_planetary bit and is a no-op on a second
// pass. Opt-in via INTEGRATION_DATABASE_URL; rolls back so the live DB
// stays clean. Reserved-range ids (990950+) avoid colliding with chain data.
func TestApplyDefenderPlanetary_CorrectsAndIdempotent(t *testing.T) {
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const (
		planetID    = "2-990950"
		structID    = "5-990951"
		defenderID  = "5-990950"
		structIndex = 990951
	)

	// Minimal planet + planet-located struct so the matched UPDATE can see
	// location_type='planet'. Avoids the event handlers (backfill has no
	// events import) — raw inserts are enough for this SQL check.
	if _, err := tx.Exec(ctx,
		`INSERT INTO structs.planet (id, owner, creator, max_ore, space_slots, air_slots, land_slots, water_slots, status, created_at, updated_at)
		 VALUES ($1, 'structs1owner', 'structs1owner', 100, 0, 0, 0, 0, 'active', NOW(), NOW())
		 ON CONFLICT (id) DO NOTHING`, planetID); err != nil {
		t.Fatalf("seed planet: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO structs.struct (id, index, type, creator, owner, location_type, location_id, operating_ambit, slot, created_at, updated_at, is_destroyed)
		 VALUES ($1, $2, 1, 'structs1owner', 'structs1owner', 'planet', $3, 'land', 1, NOW(), NOW(), FALSE)
		 ON CONFLICT (id) DO UPDATE
		    SET location_type='planet', location_id=EXCLUDED.location_id, is_destroyed=FALSE`,
		structID, structIndex, planetID); err != nil {
		t.Fatalf("seed struct: %v", err)
	}
	// Wrong flag: planet-located but is_planetary=false.
	if _, err := tx.Exec(ctx,
		`INSERT INTO structs.struct_defender (defending_struct_id, protected_struct_id, is_planetary, updated_at)
		 VALUES ($1, $2, FALSE, NOW())
		 ON CONFLICT (defending_struct_id) DO UPDATE
		    SET protected_struct_id=EXCLUDED.protected_struct_id, is_planetary=FALSE`,
		defenderID, structID); err != nil {
		t.Fatalf("seed defender: %v", err)
	}

	n, err := applyDefenderPlanetary(ctx, tx)
	if err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if n < 1 {
		t.Fatalf("pass1 updated %d rows; want >= 1", n)
	}
	var planetary bool
	if err := tx.QueryRow(ctx,
		`SELECT is_planetary FROM structs.struct_defender WHERE defending_struct_id=$1`,
		defenderID).Scan(&planetary); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !planetary {
		t.Errorf("is_planetary still false after sweep")
	}

	n2, err := applyDefenderPlanetary(ctx, tx)
	if err != nil {
		t.Fatalf("pass2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("pass2 updated %d rows; want 0 (idempotent)", n2)
	}
}

func TestFetchAllPlanetAttributes_TwoPages(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/structs/structs/planet_attribute" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("pagination.limit") != "2" {
			t.Errorf("pagination.limit = %q want 2", q.Get("pagination.limit"))
		}
		key := q.Get("pagination.key")
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "":
			next := "page2"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"planetAttributeRecords": []map[string]any{
					{"attributeId": "12-2-1", "value": "100"},
					{"attributeId": "13-2-1", "value": "200"},
				},
				"pagination": map[string]any{"next_key": next},
			})
		case "page2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"planetAttributeRecords": []map[string]any{
					{"attributeId": "12-2-2", "value": "300"},
				},
				"pagination": map[string]any{},
			})
		default:
			t.Errorf("unexpected pagination.key %q", key)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	recs, err := fetchAllPlanetAttributes(context.Background(), srv.Client(), srv.URL, 2)
	if err != nil {
		t.Fatalf("fetchAllPlanetAttributes: %v", err)
	}
	if hits != 2 {
		t.Errorf("hits = %d want 2", hits)
	}
	if len(recs) != 3 {
		t.Fatalf("len = %d want 3", len(recs))
	}
	if recs[0].AttributeID != "12-2-1" || recs[0].Value != "100" {
		t.Errorf("recs[0] = %+v", recs[0])
	}
	if recs[2].AttributeID != "12-2-2" {
		t.Errorf("recs[2] = %+v", recs[2])
	}
}

func TestPlanetAttributeRows_FiltersAndLabels(t *testing.T) {
	rows, skipped := planetAttributeRows([]payload.PlanetAttribute{
		{AttributeID: "12-2-27693", Value: "2470534"},
		{AttributeID: "13-2-27693", Value: "0"},
		{AttributeID: "12-5-1", Value: "1"},     // struct object type
		{AttributeID: "99-2-1", Value: "1"},      // unknown attr type
		{AttributeID: "12-2-1-extra", Value: "1"}, // wrong arity
		{AttributeID: "12-2-bad", Value: "x"},    // bad value
		{AttributeID: "0-2-1", Value: ""},        // empty → 0
	})
	if skipped != 4 {
		t.Errorf("skipped = %d want 4", skipped)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d want 3", len(rows))
	}
	if rows[0].ID != "12-2-27693" || rows[0].ObjectID != "2-27693" ||
		rows[0].AttributeType != "blockStartOreMine" || rows[0].Val != 2470534 {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1].AttributeType != "blockStartOreRefine" || rows[1].Val != 0 {
		t.Errorf("rows[1] = %+v", rows[1])
	}
	if rows[2].AttributeType != "planetaryShield" || rows[2].Val != 0 {
		t.Errorf("rows[2] = %+v", rows[2])
	}
}
