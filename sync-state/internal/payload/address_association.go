package payload

// AddressAssociation matches
// structs.structs.EventAddressAssociation.addressAssociation.
//
// SQL handler: cache.handle_event_address_association
// (cache-trigger-add-queue-20260223-add-player-address.sql:5-52, which
// supersedes the earlier 20260121-bigly-refactor.sql:1576-1619 body to
// add a player-exists guard before writing).
//
// player_id on the target row is composed in Go as "1-" + PlayerIndex,
// mirroring the SQL `'1-' || v.player_index::CHARACTER VARYING` concat.
type AddressAssociation struct {
	Address            string  `json:"address"`
	PlayerIndex        JSONInt `json:"playerIndex"`
	RegistrationStatus string  `json:"registrationStatus"`
}
