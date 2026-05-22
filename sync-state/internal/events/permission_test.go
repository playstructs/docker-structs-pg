package events

import (
	"testing"
)

func TestSplitPart(t *testing.T) {
	cases := []struct {
		s, sep string
		n      int
		want   string
	}{
		{"a-b-c", "-", 1, "a"},
		{"a-b-c", "-", 2, "b"},
		{"a-b-c", "-", 3, "c"},
		{"a-b-c", "-", 4, ""},
		{"a-b-c", "-", 0, ""},
		{"", "-", 1, ""},
		{"foo", "@", 1, "foo"},
		{"foo", "@", 2, ""},
		{"foo@bar", "@", 2, "bar"},
	}
	for _, tc := range cases {
		got := splitPart(tc.s, tc.sep, tc.n)
		if got != tc.want {
			t.Errorf("splitPart(%q,%q,%d) = %q want %q", tc.s, tc.sep, tc.n, got, tc.want)
		}
	}
}

func TestDecodePermissionID_StructType(t *testing.T) {
	// Grammar for non-address types: {typeId}-{objectIndex}@{objectId}@{playerId}
	// In practice the "objectId" segment already includes the typeId
	// (e.g. "5-42"), so split_part(id, '@', 1) returns the full
	// "{type}-{index}" string.
	d, err := decodePermissionID("5-42@5-42@1-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.typeID != "5" {
		t.Errorf("typeID = %q want 5", d.typeID)
	}
	if d.objectType != "struct" {
		t.Errorf("objectType = %q want struct", d.objectType)
	}
	if d.objectIndex != "42" {
		t.Errorf("objectIndex = %q want 42", d.objectIndex)
	}
	if d.objectIDLiteral != "5-42" {
		t.Errorf("objectID = %q want 5-42", d.objectIDLiteral)
	}
	if d.playerIDLiteral != "5-42" {
		// SQL behavior — split_part('5-42@5-42@1-1','@',2) = '5-42'.
		// The grammar in practice may use this for symmetric pairing.
		t.Errorf("playerID = %q want 5-42", d.playerIDLiteral)
	}
}

func TestDecodePermissionID_SimpleStruct(t *testing.T) {
	// Production-shape: {typeId}-{objectIndex}@{playerId}
	d, err := decodePermissionID("5-42@1-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.typeID != "5" || d.objectType != "struct" {
		t.Errorf("type: %+v", d)
	}
	if d.objectIndex != "42" {
		t.Errorf("objectIndex = %q want 42", d.objectIndex)
	}
	if d.objectIDLiteral != "5-42" {
		t.Errorf("objectID = %q want 5-42", d.objectIDLiteral)
	}
	if d.playerIDLiteral != "1-1" {
		t.Errorf("playerID = %q want 1-1", d.playerIDLiteral)
	}
}

func TestDecodePermissionID_AddressType(t *testing.T) {
	d, err := decodePermissionID("8-structs1abc")
	if err != nil {
		t.Fatal(err)
	}
	if d.typeID != "8" || d.objectType != "address" {
		t.Errorf("type: %+v", d)
	}
	if d.objectIndex != "structs1abc" {
		t.Errorf("objectIndex = %q want structs1abc", d.objectIndex)
	}
	// objectIDLiteral and playerIDLiteral are not used by the handler
	// for type 8 (they're replaced with a player_address lookup), but
	// the decoder still populates them for fidelity with split_part.
}

func TestDecodePermissionID_AllTypes(t *testing.T) {
	types := map[string]string{
		"0":  "guild",
		"1":  "player",
		"2":  "planet",
		"3":  "reactor",
		"4":  "substation",
		"5":  "struct",
		"6":  "allocation",
		"7":  "infusion",
		"8":  "address",
		"9":  "fleet",
		"10": "provider",
		"11": "agreement",
	}
	for prefix, want := range types {
		d, _ := decodePermissionID(prefix + "-x@y@z")
		if d.objectType != want {
			t.Errorf("type %s: got %q want %q", prefix, d.objectType, want)
		}
	}
}

func TestDecodePermissionID_UnknownType(t *testing.T) {
	// SQL CASE returns NULL for prefixes outside 0..11; we mirror that
	// by leaving objectType empty.
	d, _ := decodePermissionID("99-x@y@z")
	if d.objectType != "" {
		t.Errorf("unknown type should be empty, got %q", d.objectType)
	}
}

func TestDecodePermissionID_Empty(t *testing.T) {
	if _, err := decodePermissionID(""); err == nil {
		t.Error("expected error on empty id")
	}
}
