package payload

import (
	"encoding/json"
	"testing"
)

func TestJSONIntAcceptsNumberOrString(t *testing.T) {
	cases := map[string]int64{
		`123`:       123,
		`"456"`:     456,
		`-7`:        -7,
		`"0"`:       0,
		`null`:      0,
		`""`:        0,
		`"   12  "`: 12,
	}
	for in, want := range cases {
		var j JSONInt
		if err := json.Unmarshal([]byte(in), &j); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if int64(j) != want {
			t.Errorf("%q -> %d, want %d", in, j, want)
		}
	}
}

func TestJSONIntRejectsGarbage(t *testing.T) {
	var j JSONInt
	if err := json.Unmarshal([]byte(`"not a number"`), &j); err == nil {
		t.Fatal("expected error on garbage")
	}
}

func TestNumericPreservesPrecision(t *testing.T) {
	// 70-digit value that float64 would lose.
	big := `1234567890123456789012345678901234567890`
	var n Numeric
	if err := json.Unmarshal([]byte(`"`+big+`"`), &n); err != nil {
		t.Fatal(err)
	}
	if n.String() != big {
		t.Errorf("got %q want %q", n.String(), big)
	}
	if v := n.PgValue(); v != big {
		t.Errorf("PgValue = %v, want %q", v, big)
	}
	// Empty -> nil for SQL NULL
	var empty Numeric
	if v := empty.PgValue(); v != nil {
		t.Errorf("empty.PgValue = %v, want nil", v)
	}
}

func TestJSONBoolFlexInputs(t *testing.T) {
	cases := map[string]bool{
		`true`:    true,
		`false`:   false,
		`"true"`:  true,
		`"false"`: false,
		`"1"`:     true,
		`"0"`:     false,
		`null`:    false,
		`""`:      false,
	}
	for in, want := range cases {
		var b JSONBool
		if err := json.Unmarshal([]byte(in), &b); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if bool(b) != want {
			t.Errorf("%q -> %v, want %v", in, b, want)
		}
	}
}

func TestDecodeUnwrapsStringWrappedJSON(t *testing.T) {
	// Cosmos chain attribute "value" sometimes arrives as a JSON-encoded
	// string. Decode should unwrap that.
	inner := `{"id":"1-1","index":7,"endpoint":"http://example"}`
	wrapped, _ := json.Marshal(inner)
	g, err := Decode[Guild](wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "1-1" || g.Endpoint != "http://example" || g.Index != 7 {
		t.Fatalf("decoded wrong: %+v", g)
	}
	// Direct object form should also work.
	g2, err := Decode[Guild]([]byte(inner))
	if err != nil {
		t.Fatal(err)
	}
	if g2.ID != "1-1" {
		t.Fatalf("direct decode wrong: %+v", g2)
	}
}

func TestStructTypeV021CanDefendIsDecoded(t *testing.T) {
	p, err := Decode[StructType](json.RawMessage(`{"id":"7","type":"Miner","canDefend":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID.Int64() != 7 || p.Type != "Miner" || !p.CanDefend.Bool() {
		t.Fatalf("unexpected decoded payload: %+v", p)
	}
}

func TestGuildV021ExtraFieldsAreIgnored(t *testing.T) {
	p, err := Decode[Guild](json.RawMessage(`{
		"id":"0-9","index":"9","name":"Nine",
		"bankConvertInFee":"10","bankConvertOutFee":"20","charterSolverId":"1-1"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "0-9" || p.Name != "Nine" {
		t.Fatalf("unexpected guild: %+v", p)
	}
}

func TestGuildBankAddressV021TokenPoolIsIgnored(t *testing.T) {
	p, err := Decode[GuildBankAddress](json.RawMessage(`{
		"guildId":"0-9","bankCollateralPool":"structs1pool","bankTokenPool":"structs1token"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.GuildID != "0-9" || p.BankCollateralPool != "structs1pool" {
		t.Fatalf("unexpected guild bank address: %+v", p)
	}
}

func TestDecodePlayerGuildRank(t *testing.T) {
	// protojson emits uint64 as a quoted string; also accept bare number.
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{"quoted", `{"id":"1-1","guildRank":"101"}`, 101},
		{"number", `{"id":"1-1","guildRank":42}`, 42},
		{"zero", `{"id":"1-1","guildRank":"0"}`, 0},
		{"missing", `{"id":"1-1"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Decode[Player]([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if p.GuildRank.Int64() != tc.want {
				t.Errorf("GuildRank = %d want %d", p.GuildRank.Int64(), tc.want)
			}
		})
	}
}

func TestStructTypeIsCommand(t *testing.T) {
	if !(StructType{Class: "Command Ship"}).IsCommand() {
		t.Error("Command Ship should be command")
	}
	if (StructType{Class: "Destroyer"}).IsCommand() {
		t.Error("Destroyer should not be command")
	}
}
