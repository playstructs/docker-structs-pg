package sync

import (
	"encoding/json"
	"testing"
)

func TestEncodeAttributeValuePassesJSONStrings(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`5-262312`, `"5-262312"`},
		{`"5-262312"`, `"5-262312"`},
		{`{"objectId":"11-42"}`, `{"objectId":"11-42"}`},
		{`[1,2]`, `[1,2]`},
	}
	for _, tc := range cases {
		got := string(encodeAttributeValue(tc.in))
		if got != tc.want {
			t.Errorf("encodeAttributeValue(%q) = %s want %s", tc.in, got, tc.want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("encodeAttributeValue(%q) produced invalid JSON %s", tc.in, got)
		}
	}
}
