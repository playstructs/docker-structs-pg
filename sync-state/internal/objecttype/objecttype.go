// Package objecttype is the Go port of structs.GET_OBJECT_TYPE and
// structs.GET_OBJECT_ID from references/structs-pg/deploy/table-stat.sql.
//
// The object_type enum has 12 stable entries, ordered by integer id 0..11.
// Both lookups are stateless string manipulation, so doing them in Go saves
// a DB round trip per stat row in the grid / struct_attribute handlers
// (which can produce multiple stat rows per event).
package objecttype

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind is the ObjectType enum value as a typed constant.
type Kind uint8

// MUST match the ORDER of structs.object_type in
// references/structs-pg/deploy/table-stat.sql:5-18.
const (
	Guild Kind = iota
	Player
	Planet
	Reactor
	Substation
	Struct
	Allocation
	Infusion
	Address
	Fleet
	Provider
	Agreement
)

// numKinds is the count of defined enum entries.
const numKinds = 12

var labels = [numKinds]string{
	"guild",
	"player",
	"planet",
	"reactor",
	"substation",
	"struct",
	"allocation",
	"infusion",
	"address",
	"fleet",
	"provider",
	"agreement",
}

// Label returns the canonical enum label, identical to the PG
// structs.object_type cast result.
func (k Kind) Label() string {
	if int(k) >= numKinds {
		return ""
	}
	return labels[k]
}

// String returns the same as Label (so Kind is fmt-friendly).
func (k Kind) String() string { return k.Label() }

// FromID is the Go equivalent of structs.GET_OBJECT_TYPE(object_id INTEGER).
// Returns (Kind, true) for valid ids 0..11, (0, false) otherwise.
func FromID(id int) (Kind, bool) {
	if id < 0 || id >= numKinds {
		return 0, false
	}
	return Kind(id), true
}

// FromLabel is the inverse lookup ("guild" -> 0).
func FromLabel(label string) (Kind, bool) {
	for i, l := range labels {
		if l == label {
			return Kind(i), true
		}
	}
	return 0, false
}

// Format is the Go equivalent of structs.GET_OBJECT_ID(object_type, index):
// "<label_id>-<index>". E.g. Format(Planet, 42) == "2-42".
func Format(k Kind, index int) string {
	return strconv.Itoa(int(k)) + "-" + strconv.Itoa(index)
}

// Parse splits "<label_id>-<index>" or "<label_id>-<index>-<...>" and returns
// the leading Kind plus the leftover suffix (everything after the second
// dash, or "" if there isn't one). This matches the attribute-id parsing
// pattern used in handle_event_grid and handle_event_struct_attribute.
//
// Returns an error if the leading id is not a valid object_type.
func Parse(s string) (Kind, int, string, error) {
	parts := strings.SplitN(s, "-", 3)
	if len(parts) < 2 {
		return 0, 0, "", fmt.Errorf("objecttype.Parse: %q missing dash", s)
	}
	idNum, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, "", fmt.Errorf("objecttype.Parse: %q bad type id: %w", s, err)
	}
	k, ok := FromID(idNum)
	if !ok {
		return 0, 0, "", fmt.Errorf("objecttype.Parse: %q type id %d out of range", s, idNum)
	}
	idx, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, "", fmt.Errorf("objecttype.Parse: %q bad index: %w", s, err)
	}
	suffix := ""
	if len(parts) == 3 {
		suffix = parts[2]
	}
	return k, idx, suffix, nil
}

// All returns every defined Kind in id order. Useful for iteration in tests
// and ALTER-loop generators.
func All() []Kind {
	out := make([]Kind, numKinds)
	for i := range out {
		out[i] = Kind(i)
	}
	return out
}
