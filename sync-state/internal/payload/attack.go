package payload

import "encoding/json"

// Attack matches structs.structs.EventAttack.eventAttackDetail.
// SQL handler: cache.handle_event_attack
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1961-1988)
//
// The SQL handler only extracts attackerStructId for its own logic but
// stores the FULL payload as planet_activity.detail. We capture the same
// shape: a typed AttackerStructID for the planet-resolution path plus the
// raw JSON for the detail column.
//
// Raw is preserved so we don't lose any chain-added fields (combat damage,
// defender list, etc.) — the SQL handler did the same.
type Attack struct {
	AttackerStructID string          `json:"attackerStructId"`
	Raw              json.RawMessage `json:"-"`
}
