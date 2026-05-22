package payload

// Substation matches structs.structs.EventSubstation.substation.
// SQL handler: cache.handle_event_substation
// (cache-trigger-add-queue-20260427-ugc-fields.sql:254-304)
//
// name/pfp live directly on structs.substation (no separate meta table).
type Substation struct {
	ID      string `json:"id"`
	Owner   string `json:"owner"`
	Creator string `json:"creator"`
	Name    string `json:"name"`
	PFP     string `json:"pfp"`
}
