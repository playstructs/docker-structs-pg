package payload

import (
	"encoding/json"
	"testing"
)

func TestJSONIntAcceptsNumberOrString(t *testing.T) {
	cases := map[string]int64{
		`123`:        123,
		`"456"`:      456,
		`-7`:         -7,
		`"0"`:        0,
		`null`:       0,
		`""`:         0,
		`"   12  "`:  12,
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
		`true`:  true,
		`false`: false,
		`"true"`: true,
		`"false"`: false,
		`"1"`:    true,
		`"0"`:    false,
		`null`:   false,
		`""`:     false,
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

func TestStructTypeIsCommand(t *testing.T) {
	if !(StructType{Class: "Command Ship"}).IsCommand() {
		t.Error("Command Ship should be command")
	}
	if (StructType{Class: "Destroyer"}).IsCommand() {
		t.Error("Destroyer should not be command")
	}
}
