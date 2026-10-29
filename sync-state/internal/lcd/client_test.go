package lcd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPagesKeepHeightAcrossEmptyPage(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("x-cosmos-block-height") != "101" {
			t.Errorf("request lost explicit height")
		}
		w.Header().Set("Grpc-Metadata-X-Cosmos-Block-Height", "101")
		if r.URL.Query().Get("pagination.key") == "" {
			_, _ = w.Write([]byte(`{"rows":[],"pagination":{"next_key":"a+/="}}`))
		} else {
			if r.URL.Query().Get("pagination.key") != "a+/=" {
				t.Error("pagination key did not round trip")
			}
			_, _ = w.Write([]byte(`{"rows":[{"value":"18446744073709551615"}],"pagination":{"next_key":null}}`))
		}
	}))
	defer srv.Close()
	var values []string
	err := (Client{BaseURL: srv.URL, HTTP: srv.Client()}).Pages(context.Background(), "/rows", "rows", 101, 2, func(raw json.RawMessage) error {
		var row struct{ Value string }
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		values = append(values, row.Value)
		return nil
	})
	if err != nil || hits != 2 || len(values) != 1 || values[0] != "18446744073709551615" {
		t.Fatalf("incomplete snapshot: hits=%d values=%v err=%v", hits, values, err)
	}
}

func TestPagesRefuseIncompleteSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, body, height, secondHeight, want string
		status                                 int
	}{
		{"missing height", `{"rows":[],"pagination":{}}`, "", "", "no response height", 200},
		{"wrong height", `{"rows":[],"pagination":{}}`, "102", "", "requested height", 200},
		{"contradictory headers", `{"rows":[],"pagination":{}}`, "101", "102", "requested height", 200},
		{"pruned", `{}`, "101", "", "HTTP 400", 400},
		{"missing collection", `{"pagination":{}}`, "101", "", "missing collection", 200},
		{"null collection", `{"rows":null,"pagination":{}}`, "101", "", "missing collection", 200},
		{"missing pagination", `{"rows":[]}`, "101", "", "pagination", 200},
		{"null pagination", `{"rows":[],"pagination":null}`, "101", "", "pagination", 200},
		{"repeated key", `{"rows":[],"pagination":{"next_key":"loop"}}`, "101", "", "repeated pagination key", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.height != "" {
					w.Header().Set("X-Cosmos-Block-Height", tc.height)
				}
				if tc.secondHeight != "" {
					w.Header().Set("Grpc-Metadata-X-Cosmos-Block-Height", tc.secondHeight)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := (Client{BaseURL: srv.URL, HTTP: srv.Client()}).Pages(context.Background(), "/rows", "rows", 101, 2, func(json.RawMessage) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestValidateChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"default_node_info":{"network":"structstestnet-111"}}`))
	}))
	defer srv.Close()
	client := Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := client.ValidateChain(context.Background(), "structstestnet-111"); err != nil {
		t.Fatal(err)
	}
	if err := client.ValidateChain(context.Background(), "wrong-chain"); err == nil {
		t.Fatal("wrong chain accepted")
	}
}
