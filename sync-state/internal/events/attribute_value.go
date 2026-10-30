package events

import (
	"fmt"
	"strconv"
)

// Attributes use uint64 on chain. An empty legacy event clears the value;
// signs, fractions and overflow are invalid rather than a wrapped value.
func parseAttributeValue(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("invalid unsigned attribute %q", value)
		}
	}
	return strconv.ParseUint(value, 10, 64)
}
