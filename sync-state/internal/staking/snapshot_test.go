package staking

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"sync-state/internal/lcd"
)

const testAddress = "structs1q3nx9j4qcqaf4p2qjc9zxqxrccg4renc2fyaw5"
const validatorA = "structsvaloper1pa4ch99mktue309avws5xxp70t4rr5egxfmcmp"
const validatorB = "structsvaloper1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
const validatorC = "structsvaloper1rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"

var testLimits = Limits{PageSize: 2, MaxPairs: 10, MaxEntries: 10}

func delegation(validator, amount string) map[string]any {
	return map[string]any{
		"delegation": map[string]any{"delegator_address": testAddress, "validator_address": validator, "shares": "0.123456789012345678"},
		"balance":    map[string]any{"denom": "ualpha", "amount": amount},
	}
}

func pendingEntry(id, initial, balance, hold string) map[string]any {
	return map[string]any{"creation_height": "100", "completion_time": "2000-01-01T00:00:00Z",
		"initial_balance": initial, "balance": balance, "unbonding_id": id, "unbonding_on_hold_ref_count": hold}
}

func unbondingRow(entries ...map[string]any) map[string]any {
	return map[string]any{"delegator_address": testAddress, "validator_address": validatorB, "entries": entries}
}

func redelegationRow() map[string]any {
	entry := pendingEntry("18446744073709551615", "600", "250", "0")
	delete(entry, "balance")
	entry["shares_dst"] = "0.123456789012345678"
	return map[string]any{
		// SDK NewRedelegationResponse leaves this nested entries field empty.
		"redelegation": map[string]any{"delegator_address": testAddress, "validator_src_address": validatorC,
			"validator_dst_address": validatorA, "entries": []any{}},
		"entries": []any{map[string]any{"redelegation_entry": entry, "balance": "250"}},
	}
}

type fixture struct {
	delegations, unbondings, redelegations []any
}

func newFixture() fixture {
	return fixture{
		delegations:   []any{delegation(validatorA, "18446744073709551616")},
		unbondings:    []any{unbondingRow(pendingEntry("1", "500", "300", "0"), pendingEntry("2", "50", "40", "2"))},
		redelegations: []any{redelegationRow()},
	}
}

func serveFixture(t *testing.T, f fixture, mutate func(*http.Request, map[string]any)) lcd.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cosmos/base/tendermint/v1beta1/node_info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"default_node_info": map[string]any{"network": "structstestnet-111"}})
			return
		}
		if r.Header.Get("x-cosmos-block-height") != "3353000" {
			t.Error("staking request lost pinned height")
		}
		if r.URL.Query().Get("pagination.limit") != "2" {
			t.Error("staking request lost page limit")
		}
		w.Header().Set("X-Cosmos-Block-Height", "3353000")
		body := map[string]any{"pagination": map[string]any{"next_key": nil}}
		switch r.URL.Path {
		case "/cosmos/staking/v1beta1/delegations/" + testAddress:
			body["delegation_responses"] = f.delegations
		case "/cosmos/staking/v1beta1/delegators/" + testAddress + "/unbonding_delegations":
			body["unbonding_responses"] = f.unbondings
		case "/cosmos/staking/v1beta1/delegators/" + testAddress + "/redelegations":
			body["redelegation_responses"] = f.redelegations
		default:
			t.Errorf("unexpected unscoped path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if mutate != nil {
			mutate(r, body)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return lcd.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestNativeSnapshotCurrentClaims(t *testing.T) {
	client := serveFixture(t, newFixture(), nil)
	s, err := Fetch(context.Background(), client, "structstestnet-111", testAddress, 3353000, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	// A slashed unbonding uses 300, not initial 500. An overdue held entry
	// remains 40. Redelegation balance 250 is already in destination stake.
	if s.Bonded != "18446744073709551616" || s.Unbonding != "340" {
		t.Fatalf("wrong current claims: bonded=%s unbonding=%s", s.Bonded, s.Unbonding)
	}
	if len(s.Unbondings) != 2 || s.Unbondings[1].HoldCount != 2 || s.Unbondings[1].InitialBalance != "50" {
		t.Fatalf("lost held entry: %+v", s.Unbondings)
	}
	if len(s.Redelegations) != 1 || s.Redelegations[0].ID != ^uint64(0) || s.Redelegations[0].Balance != "250" {
		t.Fatalf("lost exact redelegation metadata: %+v", s.Redelegations)
	}
	if !reflect.DeepEqual(s.Validators(), []string{validatorA, validatorB, validatorC}) {
		t.Fatalf("source-validator slash exposure lost: %v", s.Validators())
	}
}

func TestNativeSnapshotCompletionAndEmptyState(t *testing.T) {
	f := newFixture()
	f.unbondings = []any{}
	f.redelegations = []any{}
	s, err := Fetch(context.Background(), serveFixture(t, f, nil), "structstestnet-111", testAddress, 3353000, testLimits)
	if err != nil || s.Unbonding != "0" || s.Bonded != "18446744073709551616" {
		t.Fatalf("completed claim retained: snapshot=%+v err=%v", s, err)
	}
	f.delegations = []any{}
	s, err = Fetch(context.Background(), serveFixture(t, f, nil), "structstestnet-111", testAddress, 3353000, testLimits)
	if err != nil || s.Bonded != "0" || s.Unbonding != "0" || len(s.Validators()) != 0 {
		t.Fatalf("complete empty snapshot: snapshot=%+v err=%v", s, err)
	}
}

func TestNativeSnapshotPaginationAndFailure(t *testing.T) {
	for _, lateFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("lateFailure=%t", lateFailure), func(t *testing.T) {
			client := serveFixture(t, newFixture(), func(r *http.Request, body map[string]any) {
				if !strings.Contains(r.URL.Path, "/delegations/") {
					return
				}
				if r.URL.Query().Get("pagination.key") == "" {
					body["delegation_responses"] = []any{}
					body["pagination"] = map[string]any{"next_key": "continue+/="}
				} else if lateFailure {
					body["delegation_responses"] = []any{delegation(validatorA, "1.5")}
				}
			})
			s, err := Fetch(context.Background(), client, "structstestnet-111", testAddress, 3353000, testLimits)
			if lateFailure {
				if err == nil || s != nil {
					t.Fatalf("partial snapshot returned: %+v %v", s, err)
				}
			} else if err != nil || s.Bonded != "18446744073709551616" {
				t.Fatalf("lost continuation after empty page: %+v %v", s, err)
			}
		})
	}
}

func TestNativeSnapshotRejectsIncompleteOrInvalidState(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*fixture)
	}{
		{"duplicate pair", func(f *fixture) { f.delegations = append(f.delegations, f.delegations[0]) }},
		{"wrong delegator", func(f *fixture) {
			f.delegations[0].(map[string]any)["delegation"].(map[string]any)["delegator_address"] = "structs1qqq"
		}},
		{"wrong denom", func(f *fixture) { f.delegations[0].(map[string]any)["balance"].(map[string]any)["denom"] = "other" }},
		{"missing coin", func(f *fixture) { delete(f.delegations[0].(map[string]any), "balance") }},
		{"null amount", func(f *fixture) { f.delegations[0].(map[string]any)["balance"].(map[string]any)["amount"] = nil }},
		{"negative amount", func(f *fixture) { f.delegations = []any{delegation(validatorA, "-1")} }},
		{"fractional amount", func(f *fixture) { f.delegations = []any{delegation(validatorA, "0.5")} }},
		{"exponent amount", func(f *fixture) { f.delegations = []any{delegation(validatorA, "1e6")} }},
		{"SDK amount overflow", func(f *fixture) {
			f.delegations = []any{delegation(validatorA, new(big.Int).Lsh(big.NewInt(1), 256).String())}
		}},
		{"invalid shares", func(f *fixture) {
			f.delegations[0].(map[string]any)["delegation"].(map[string]any)["shares"] = "1.0000000000000000001"
		}},
		{"missing unbondings", func(f *fixture) { f.unbondings = nil }},
		{"duplicate entry ID", func(f *fixture) {
			f.unbondings = []any{unbondingRow(pendingEntry("9", "10", "10", "0"), pendingEntry("9", "20", "20", "1"))}
		}},
		{"future creation", func(f *fixture) {
			e := pendingEntry("1", "10", "10", "0")
			e["creation_height"] = "3353001"
			f.unbondings = []any{unbondingRow(e)}
		}},
		{"missing hold", func(f *fixture) {
			e := pendingEntry("1", "10", "10", "0")
			delete(e, "unbonding_on_hold_ref_count")
			f.unbondings = []any{unbondingRow(e)}
		}},
		{"invalid hold", func(f *fixture) { f.unbondings = []any{unbondingRow(pendingEntry("1", "10", "10", "-1"))} }},
		{"excess unbonding balance", func(f *fixture) { f.unbondings = []any{unbondingRow(pendingEntry("1", "10", "11", "0"))} }},
		{"missing redelegation responses", func(f *fixture) { f.redelegations[0].(map[string]any)["entries"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.edit(&f)
			s, err := Fetch(context.Background(), serveFixture(t, f, nil), "structstestnet-111", testAddress, 3353000, testLimits)
			if err == nil || s != nil {
				t.Fatalf("invalid state produced snapshot: %+v %v", s, err)
			}
		})
	}
}

func TestNativeSnapshotLimitsAndChain(t *testing.T) {
	client := serveFixture(t, newFixture(), nil)
	for _, limits := range []Limits{{PageSize: 2, MaxPairs: 1, MaxEntries: 10}, {PageSize: 2, MaxPairs: 10, MaxEntries: 1}, {}} {
		if s, err := Fetch(context.Background(), client, "structstestnet-111", testAddress, 3353000, limits); err == nil || s != nil {
			t.Fatalf("limit failure returned partial snapshot: %+v %v", s, err)
		}
	}
	if s, err := Fetch(context.Background(), client, "wrong-chain", testAddress, 3353000, testLimits); err == nil || s != nil {
		t.Fatal("wrong-chain snapshot accepted")
	}
}

// Opt-in, read-only probe. The caller selects a public address and an explicit
// height; this test neither reads credentials nor submits any transactions.
func TestNativeSnapshotLiveGateway(t *testing.T) {
	base := os.Getenv("INTEGRATION_LCD_URL")
	if base == "" {
		t.Skip("INTEGRATION_LCD_URL not set")
	}
	height, err := strconv.ParseInt(os.Getenv("INTEGRATION_LCD_HEIGHT"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Fetch(ctx, lcd.Client{BaseURL: base, HTTP: &http.Client{Timeout: 10 * time.Second}},
		os.Getenv("INTEGRATION_LCD_CHAIN"), os.Getenv("INTEGRATION_LCD_ADDRESS"), height,
		Limits{PageSize: 100, MaxPairs: 1000, MaxEntries: 10000})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verified chain=%s height=%d delegations=%d unbondings=%d redelegations=%d bonded=%s pending=%s",
		s.ChainID, s.Height, len(s.Delegations), len(s.Unbondings), len(s.Redelegations), s.Bonded, s.Unbonding)
}
