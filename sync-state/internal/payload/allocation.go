package payload

// Allocation matches structs.structs.EventAllocation.allocation.
// SQL handler: cache.handle_event_allocation
// (cache-trigger-add-queue-20260325-110b-schema-update.sql:5-62)
type Allocation struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"`
	SourceObjectID string  `json:"sourceObjectId"`
	Index          JSONInt `json:"index"`
	DestinationID  string  `json:"destinationId"`
	Creator        string  `json:"creator"`
	Controller     string  `json:"controller"`
}
