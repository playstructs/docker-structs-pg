package payload

import (
	"encoding/json"
	"testing"
)

func TestUint64Exact(t *testing.T) {
	for _, s := range []string{"0", "9007199254740993", "9223372036854775808", "18446744073709551615"} {
		for _, raw := range []string{s, `"` + s + `"`} {
			var n Uint64
			if err := json.Unmarshal([]byte(raw), &n); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(n)
			if err != nil || string(encoded) != `"`+s+`"` || n.PgValue() != s {
				t.Fatalf("lost precision: %s => %s (%v)", raw, encoded, err)
			}
		}
	}
	for _, raw := range []string{`null`, `""`, `" 1"`, `"+1"`, `-1`, `1.5`, `1e3`, `"NaN"`, `"Infinity"`, `"18446744073709551616"`} {
		var n Uint64
		if err := json.Unmarshal([]byte(raw), &n); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestRequiredNumericPresence(t *testing.T) {
	type row struct {
		Value Uint64  `json:"value"`
		Power Natural `json:"power"`
	}
	for _, raw := range []string{`null`, `{}`, `{"value":null,"power":"0"}`, `{"value":"0","power":null}`} {
		if _, err := DecodeRequired[row]([]byte(raw), "value", "power"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	r, err := DecodeRequired[row]([]byte(`{"value":"0","power":"340282366920938463463374607431768211456","future":true}`), "value", "power")
	if err != nil || r.Value != 0 || r.Power.PgValue() != "340282366920938463463374607431768211456" {
		t.Fatalf("valid zero/large/additive payload: %+v %v", r, err)
	}
}
