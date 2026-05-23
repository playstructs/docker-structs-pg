package genesis

import (
	"strings"
	"testing"
)

// TestParse_RejectsEmpty guards the common "RPC returned 0 bytes"
// failure mode that used to silently produce an empty Document.
func TestParse_RejectsEmpty(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatalf("expected error on nil input")
	}
	if _, err := Parse([]byte{}); err == nil {
		t.Fatalf("expected error on empty input")
	}
}

// TestParse_RequiresChainID + TestParse_RequiresGenesisTime guard
// against half-populated genesis files (the kind a misconfigured proxy
// might serve a stale slice of).
func TestParse_RequiresChainID(t *testing.T) {
	_, err := Parse([]byte(`{"genesis_time":"2026-01-01T00:00:00Z"}`))
	if err == nil || !strings.Contains(err.Error(), "chain_id") {
		t.Fatalf("want chain_id error, got %v", err)
	}
}

func TestParse_RequiresGenesisTime(t *testing.T) {
	_, err := Parse([]byte(`{"chain_id":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "genesis_time") {
		t.Fatalf("want genesis_time error, got %v", err)
	}
}

func TestParse_Minimal(t *testing.T) {
	raw := []byte(`{
		"genesis_time":"2026-01-01T00:00:00Z",
		"chain_id":"structstestnet-111",
		"app_state":{
			"bank":{"balances":[{"address":"a1","coins":[{"denom":"ualpha","amount":"10"}]}]},
			"staking":{
				"validators":[{"operator_address":"v1","tokens":"100","delegator_shares":"100.000"}],
				"delegations":[{"delegator_address":"d1","validator_address":"v1","shares":"50.000"}],
				"unbonding_delegations":[]
			},
			"structs":{
				"playerList":[{"index":"1","primaryAddress":"a1"}],
				"gridList":[{"attributeId":"0-1-1","value":"500"}]
			}
		}
	}`)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.ChainID != "structstestnet-111" {
		t.Fatalf("chain_id: %s", doc.ChainID)
	}
	if got := len(doc.AppState.Bank.Balances); got != 1 {
		t.Fatalf("balances: %d", got)
	}
	if got := len(doc.AppState.Staking.Delegations); got != 1 {
		t.Fatalf("delegations: %d", got)
	}
	if got := len(doc.AppState.Structs.GridList); got != 1 {
		t.Fatalf("gridList: %d", got)
	}
}

// TestDelegationAmount covers the math the shell script does inline:
// (shares * vTokens / vShares) | floor.
//
// Cases mirror what real chain data looks like — fractional shares,
// big numbers that overflow int64, the divide-by-zero defensive path,
// and the simple 1:1 case where shares == tokens.
func TestDelegationAmount(t *testing.T) {
	cases := []struct {
		name                    string
		shares, vTokens, vShares string
		want                    string
		wantErr                 bool
	}{
		{
			name:    "1:1 simple",
			shares:  "100", vTokens: "100", vShares: "100",
			want: "100",
		},
		{
			name:    "fractional shares floored",
			shares:  "33.333333333333333333", vTokens: "100", vShares: "100",
			want: "33",
		},
		{
			name:    "validator slashed: tokens < shares",
			shares:  "100", vTokens: "90", vShares: "100",
			want: "90",
		},
		{
			name:    "delegator fraction of pool",
			shares:  "50", vTokens: "200", vShares: "100",
			want: "100",
		},
		{
			name:    "big number (overflows int64)",
			shares:  "1000000000000000000", vTokens: "2000000000000000000", vShares: "1000000000000000000",
			want: "2000000000000000000",
		},
		{
			name:    "vShares == 0 returns 0 (defensive)",
			shares:  "100", vTokens: "100", vShares: "0",
			want: "0",
		},
		{
			name:    "non-numeric shares errors",
			shares:  "x", vTokens: "100", vShares: "100",
			wantErr: true,
		},
		{
			name:    "non-numeric vTokens errors",
			shares:  "100", vTokens: "x", vShares: "100",
			wantErr: true,
		},
		{
			name:    "non-numeric vShares errors",
			shares:  "100", vTokens: "100", vShares: "x",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := delegationAmount(c.shares, c.vTokens, c.vShares)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestPlayerOreFilter validates the gridList → ore credit projection
// rules: only "0-1-*" attribute IDs, skip zero-value, skip indices
// missing from playerList, skip empty addresses.
func TestPlayerOreFilter(t *testing.T) {
	raw := []byte(`{
		"genesis_time":"2026-01-01T00:00:00Z",
		"chain_id":"x",
		"app_state":{
			"bank":{"balances":[]},
			"staking":{"validators":[],"delegations":[],"unbonding_delegations":[]},
			"structs":{
				"playerList":[
					{"index":"1","primaryAddress":"addr1"},
					{"index":"2","primaryAddress":"addr2"},
					{"index":"3","primaryAddress":""}
				],
				"gridList":[
					{"attributeId":"0-1-1","value":"100"},
					{"attributeId":"0-1-2","value":"200"},
					{"attributeId":"0-1-3","value":"300"},
					{"attributeId":"0-1-4","value":"400"},
					{"attributeId":"0-1-1","value":"0"},
					{"attributeId":"0-2-1","value":"500"},
					{"attributeId":"1-1-1","value":"600"}
				]
			}
		}
	}`)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	addrByIndex := map[string]string{}
	for _, p := range doc.AppState.Structs.PlayerList {
		addrByIndex[p.Index] = p.PrimaryAddress
	}
	type want struct{ addr, amt string }
	var got []want
	for _, g := range doc.AppState.Structs.GridList {
		if !startsWith(g.AttributeID, "0-1-") {
			continue
		}
		if g.Value == "" || g.Value == "0" {
			continue
		}
		parts := splitN(g.AttributeID, "-", 3)
		if len(parts) < 3 {
			continue
		}
		addr, ok := addrByIndex[parts[2]]
		if !ok || addr == "" {
			continue
		}
		got = append(got, want{addr: addr, amt: g.Value})
	}
	expected := []want{
		{addr: "addr1", amt: "100"},
		{addr: "addr2", amt: "200"},
	}
	if len(got) != len(expected) {
		t.Fatalf("rows: got %d want %d (%v)", len(got), len(expected), got)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("row %d: got %v want %v", i, got[i], expected[i])
		}
	}
}

// startsWith / splitN are tiny local helpers so the test file doesn't
// duplicate imports the production code uses; intent is to mirror the
// actual filter logic in insertPlayerOre 1:1.
func startsWith(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }
func splitN(s, sep string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		idx := indexOf(s, sep)
		if idx < 0 {
			break
		}
		out = append(out, s[:idx])
		s = s[idx+len(sep):]
	}
	out = append(out, s)
	return out
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
