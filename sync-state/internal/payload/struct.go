package payload

// Struct matches structs.structs.EventStruct.structure.
// SQL handler: cache.handle_event_struct
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:749-827)
//
// Named with a trailing underscore-style alias because `struct` is a Go
// reserved word; consumers refer to it as payload.Struct.
type Struct struct {
	ID             string  `json:"id"`
	Index          JSONInt `json:"index"`
	Type           JSONInt `json:"type"`
	Creator        string  `json:"creator"`
	Owner          string  `json:"owner"`
	LocationType   string  `json:"locationType"`
	LocationID     string  `json:"locationId"`
	OperatingAmbit string  `json:"operatingAmbit"`
	Slot           JSONInt `json:"slot"`
}
