package payload

// StructDefender matches structs.structs.EventStructDefender.structDefender.
// SQL handler: cache.handle_event_struct_defender
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:829-863)
type StructDefender struct {
	DefendingStructID string `json:"defendingStructId"`
	ProtectedStructID string `json:"protectedStructId"`
}

// StructDefenderClear matches
// structs.structs.EventStructDefenderClear.structDefenderClearDetail.
// SQL handler: cache.handle_event_struct_defender_clear
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:865-883)
//
// Always deletes both:
//   - structs.struct_defender WHERE defending_struct_id = X
//   - structs.struct_attribute WHERE id = '5-' || X (attribute type 5 =
//     defender-related; Phase 4 attribute handler also touches this id).
type StructDefenderClear struct {
	DefendingStructID string `json:"defendingStructId"`
}
