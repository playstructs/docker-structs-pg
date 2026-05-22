package payload

// All three Phase 4 cache handlers (handle_event_grid,
// handle_event_struct_attribute, handle_event_planet_attribute) share the
// same payload shape on the chain: an "attributeId" string and a "value"
// stringified integer. The handlers themselves diverge in how they
// interpret attributeId (whether sub_index applies, which stat table to
// hit, etc.). To keep the wiring 1:1 with the registry pattern, each
// handler decodes into its own named struct that happens to share fields.

// Grid matches structs.structs.EventGrid.gridRecord.
// SQL handler: cache.handle_event_grid
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1683-1804)
//
// attributeId grammar: "{gridAttributeType}-{objectTypeId}-{objectIndex}"
// where gridAttributeType is 0..14 (see grid.go for the label map).
type Grid struct {
	AttributeID string `json:"attributeId"`
	Value       string `json:"value"` // empty string => DELETE
}

// StructAttribute matches structs.structs.EventStructAttribute.structAttributeRecord.
// SQL handler: cache.handle_event_struct_attribute
// (cache-trigger-add-queue-20260207-add-destroyed-block.sql:5-93)
//
// attributeId grammar: "{attrType}-{objectTypeId}-{objectIndex}[-{subIndex}]"
// where attrType is 0..6 (see struct_attribute.go for labels).
// subIndex is optional and defaults to 0.
//
// "" or "0" value => DELETE (the 20260203 migration added the numeric-zero
// tombstone semantics so chain-emitted proto3 defaults don't accumulate).
type StructAttribute struct {
	AttributeID string `json:"attributeId"`
	Value       string `json:"value"`
}

// PlanetAttribute matches structs.structs.EventPlanetAttribute.planetAttributeRecord.
// SQL handler: cache.handle_event_planet_attribute
// (cache-trigger-add-queue-20260203-add-new-events.sql:95-158)
//
// attributeId grammar: "{attrType}-{objectTypeId}-{objectIndex}"
// where attrType is 0..10 (see planet_attribute.go for labels).
// No sub_index column on structs.planet_attribute.
//
// "" or "0" value => DELETE.
type PlanetAttribute struct {
	AttributeID string `json:"attributeId"`
	Value       string `json:"value"`
}
