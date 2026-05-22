package reprocess

import "encoding/json"

// encodeAttributeValue mirrors sync.encodeAttributeValue so reprocess
// produces the exact byte-for-byte payload the original ingestion path
// did. We duplicate it (instead of exporting from sync) to keep
// reprocess free of an import cycle with the sync package.
//
// Rule: pass through anything that already looks like JSON, otherwise
// wrap the raw chain attribute as a JSON string literal.
func encodeAttributeValue(v string) []byte {
	t := stripWhitespace(v)
	if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		return []byte(v)
	}
	enc, _ := json.Marshal(v)
	return enc
}

func stripWhitespace(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return s[i:]
		}
	}
	return ""
}
