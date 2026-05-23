// Package payload contains one typed Go struct per chain event payload
// (one file per event). Each struct's JSON tags match the chain attribute's
// payload-key encoding — the same encoding cache.handle_event_* read via
// `payload->>'fieldName'` in the SQL handlers today.
//
// Adding a new event:
//
//  1. Add the payload struct here in <event_name>.go.
//  2. Create internal/events/<event_name>.go implementing events.Handler.
//  3. Add one line to events.AllHandlers() in internal/events/registry.go.
//
// Replay-safety rule: every field that's available in the payload MUST be
// in the struct, even if the handler ignores it. We never want a Go handler
// to do a DB lookup for something the chain already sent us — that breaks
// replay and surgical re-ingestion.
package payload

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// JSONInt is an int64 that decodes from JSON as either a number (proto32)
// or a quoted numeric string (proto64). Cosmos SDK protojson emits int64
// and uint64 as strings to preserve precision; uint32/int32 stay numeric.
// We accept both shapes so handlers never have to care which the chain
// happened to use.
//
// A missing field decodes as zero (matching proto3 default semantics).
type JSONInt int64

func (j *JSONInt) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("payload.JSONInt: %q: %w", string(data), err)
	}
	*j = JSONInt(n)
	return nil
}

// Int64 returns the underlying int64 for direct PG binding.
func (j JSONInt) Int64() int64 { return int64(j) }

// JSONBool decodes from JSON true/false or the strings "true"/"false"
// (some Cosmos modules wire bools as strings inside event attrs).
type JSONBool bool

func (b *JSONBool) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "false", "0":
		*b = false
		return nil
	case "true", "1":
		*b = true
		return nil
	}
	return fmt.Errorf("payload.JSONBool: %q", string(data))
}

// Bool returns the underlying bool.
func (b JSONBool) Bool() bool { return bool(b) }

// Numeric is a decimal value carried as a string so it can be passed
// verbatim to PG NUMERIC columns (no float round-trip). Accepts JSON
// number or JSON string.
//
// Use Numeric.PgValue() when binding to pgx — it returns nil for the
// empty / missing case, which becomes SQL NULL.
type Numeric string

func (n *Numeric) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	*n = Numeric(strings.TrimSpace(s))
	return nil
}

// PgValue returns the value to bind to pgx for a NUMERIC column. Empty
// becomes nil → SQL NULL, otherwise the decimal text passes through.
func (n Numeric) PgValue() any {
	if n == "" {
		return nil
	}
	return string(n)
}

// String returns the underlying decimal text (empty for null/missing).
func (n Numeric) String() string { return string(n) }

// Raw is a passthrough JSON value preserved for nested objects like
// fleet.space / planet.air maps. Use json.RawMessage directly in payload
// structs and let the handler do its own jsonb assembly via pgx.
type Raw = json.RawMessage

// NullableText is a no-op passthrough kept for handler readability.
//
// We initially used it to coerce empty strings to SQL NULL, but that
// diverges from the original SQL handlers — `jsonb_to_record` writes
// chain-emitted "" as the empty string, not NULL, and the IS DISTINCT
// FROM guards specifically rely on '' vs NULL being treated as different.
// The chain emits explicit values for every proto3 field (default zero
// for ints, "" for unset texts), so passing them verbatim is byte-equal
// to the SQL handler's behaviour. The helper is still useful at handler
// call sites to make intent clear ("this column may be empty") and to
// give us one place to revisit if we ever switch to pointer payloads.
func NullableText(s string) any { return s }

// NullableInt is a no-op passthrough (kept for symmetry with NullableText).
// Chain payloads emit explicit 0 for unset proto3 ints — we pass that
// through to PG, matching the SQL handler behaviour.
func NullableInt(n int64) any { return n }

// MustMarshal panics on json.Marshal error; intended for handler-built
// jsonb literals where the input is fully under our control (e.g. the
// fleet/planet space-air-land-water map). Never call with user input.
func MustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("payload.MustMarshal: %v", err))
	}
	return b
}

// Decode unmarshals raw into a freshly-zeroed T. Used by handlers as the
// first line of Handle(): payload.Decode[payload.Player](raw).
func Decode[T any](raw json.RawMessage) (T, error) {
	var p T
	if len(raw) == 0 {
		return p, fmt.Errorf("empty payload")
	}
	// Accept payloads that arrive as a JSON-encoded string (chain wraps
	// the entire message JSON in a string for some event attributes).
	if raw[0] == '"' {
		var unq string
		if err := json.Unmarshal(raw, &unq); err == nil {
			if err := json.Unmarshal([]byte(unq), &p); err != nil {
				return p, fmt.Errorf("decode %T: unwrap+unmarshal: %w", p, err)
			}
			return p, nil
		}
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("decode %T: %w", p, err)
	}
	return p, nil
}
