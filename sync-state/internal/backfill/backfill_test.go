package backfill

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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
