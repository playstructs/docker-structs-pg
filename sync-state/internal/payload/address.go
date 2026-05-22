package payload

// Address matches structs.structs.EventAddress.address.
//
// Composite key is EventAddress.address (NOT EventPlayerAddress, which
// does not exist on this chain).
//
// SQL handler: cache.handle_event_address
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1533-1574)
type Address struct {
	Address  string `json:"address"`
	PlayerID string `json:"playerId"`
}
