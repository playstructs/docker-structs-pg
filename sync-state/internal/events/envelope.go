package events

import (
	"encoding/json"
	"fmt"

	"sync-state/internal/rpc"
)

// Whole-event handlers register the event type itself. Nested legacy payloads
// retain their type.attribute key. SDK metadata is never a business field.
func Envelope(ev rpc.Event) (json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(ev.Attributes))
	for _, attr := range ev.Attributes {
		if attr.Key == "mode" || attr.Key == "msg_index" {
			continue
		}
		if _, exists := fields[attr.Key]; exists {
			return nil, fmt.Errorf("%s: duplicate attribute %s", ev.Type, attr.Key)
		}
		if !json.Valid([]byte(attr.Value)) {
			return nil, fmt.Errorf("%s.%s: invalid typed JSON", ev.Type, attr.Key)
		}
		fields[attr.Key] = json.RawMessage(attr.Value)
	}
	return json.Marshal(fields)
}

func (r *Router) ValidateNestedEvent(ev rpc.Event) (string, error) {
	for _, required := range r.nestedAttributes[ev.Type] {
		count := 0
		for _, attr := range ev.Attributes {
			if attr.Key == required {
				count++
			}
		}
		if count != 1 {
			key := ev.Type + "." + required
			return key, fmt.Errorf("%s requires exactly one payload attribute; got %d", key, count)
		}
	}
	return "", nil
}
