package objecttype

import "testing"

func TestLabels(t *testing.T) {
	want := map[Kind]string{
		Guild:      "guild",
		Player:     "player",
		Planet:     "planet",
		Reactor:    "reactor",
		Substation: "substation",
		Struct:     "struct",
		Allocation: "allocation",
		Infusion:   "infusion",
		Address:    "address",
		Fleet:      "fleet",
		Provider:   "provider",
		Agreement:  "agreement",
	}
	for k, v := range want {
		if got := k.Label(); got != v {
			t.Errorf("Kind(%d).Label() = %q, want %q", k, got, v)
		}
		if k2, ok := FromLabel(v); !ok || k2 != k {
			t.Errorf("FromLabel(%q) = (%d, %v), want (%d, true)", v, k2, ok, k)
		}
	}
}

func TestFromIDRange(t *testing.T) {
	for i := 0; i < 12; i++ {
		if _, ok := FromID(i); !ok {
			t.Errorf("FromID(%d) = false, want true", i)
		}
	}
	for _, i := range []int{-1, 12, 99} {
		if _, ok := FromID(i); ok {
			t.Errorf("FromID(%d) = true, want false", i)
		}
	}
}

func TestFormatParse(t *testing.T) {
	cases := []struct {
		k     Kind
		index int
		want  string
	}{
		{Guild, 0, "0-0"},
		{Planet, 42, "2-42"},
		{Agreement, 12345, "11-12345"},
	}
	for _, c := range cases {
		got := Format(c.k, c.index)
		if got != c.want {
			t.Errorf("Format(%d,%d) = %q, want %q", c.k, c.index, got, c.want)
		}
		k, idx, suffix, err := Parse(got)
		if err != nil {
			t.Errorf("Parse(%q) err=%v", got, err)
			continue
		}
		if k != c.k || idx != c.index || suffix != "" {
			t.Errorf("Parse(%q) = (%d,%d,%q), want (%d,%d,\"\")", got, k, idx, suffix, c.k, c.index)
		}
	}
}

func TestParseSuffix(t *testing.T) {
	// Real grid attribute IDs look like "1-42-7" (player index 42, attribute kind 7).
	k, idx, suffix, err := Parse("1-42-7")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if k != Player || idx != 42 || suffix != "7" {
		t.Errorf("Parse(\"1-42-7\") = (%d,%d,%q), want (Player,42,\"7\")", k, idx, suffix)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "x", "12-0", "-1-0", "abc-1", "1-abc"} {
		if _, _, _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) expected error", bad)
		}
	}
}
