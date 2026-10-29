package payload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Uint64 preserves the full protobuf range. Missing legacy fields default to
// zero, but an explicit null, fraction, sign or exponent is not an integer.
// Required post-upgrade fields must also be checked with DecodeRequired.
type Uint64 uint64

func (n *Uint64) UnmarshalJSON(data []byte) error {
	s, err := unsignedText(data)
	if err != nil {
		return err
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("payload.Uint64: %q: %w", s, err)
	}
	*n = Uint64(v)
	return nil
}

func (n Uint64) String() string               { return strconv.FormatUint(uint64(n), 10) }
func (n Uint64) PgValue() any                 { return n.String() }
func (n Uint64) MarshalJSON() ([]byte, error) { return json.Marshal(n.String()) }

// Natural is an arbitrary-precision nonnegative integer for diversity totals.
// It must not narrow aggregate/squared power to the uint64 field domain.
type Natural string

func (n *Natural) UnmarshalJSON(data []byte) error {
	s, err := unsignedText(data)
	if err != nil {
		return err
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		s = "0"
	}
	*n = Natural(s)
	return nil
}

func (n Natural) PgValue() any { return string(n) }

func unsignedText(data []byte) (string, error) {
	s := string(bytes.TrimSpace(data))
	if strings.HasPrefix(s, "\"") {
		if err := json.Unmarshal(data, &s); err != nil {
			return "", err
		}
	}
	if s == "" {
		return "", fmt.Errorf("empty unsigned integer")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("invalid unsigned integer %q", s)
		}
	}
	return s, nil
}

// DecodeRequired applies presence validation only to the protocol contracts
// that require it; it does not reject unrelated additive JSON properties.
func DecodeRequired[T any](raw json.RawMessage, fields ...string) (T, error) {
	var zero T
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return zero, err
	}
	if object == nil {
		return zero, fmt.Errorf("required payload is not an object")
	}
	for _, name := range fields {
		value, exists := object[name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return zero, fmt.Errorf("required field %q is missing or null", name)
		}
	}
	return Decode[T](raw)
}

// DecodeVersioned permits absent legacy extensions before activation. Once
// any extension field is present, the entire snapshot extension is required.
func DecodeVersioned[T any](raw json.RawMessage, active bool, fields ...string) (T, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		var zero T
		return zero, fmt.Errorf("versioned payload is not a JSON object: %v", err)
	}
	for _, field := range fields {
		if _, exists := object[field]; exists {
			active = true
		}
	}
	if active {
		return DecodeRequired[T](raw, fields...)
	}
	return Decode[T](raw)
}
