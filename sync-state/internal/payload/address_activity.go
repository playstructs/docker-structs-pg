package payload

import "time"

// AddressActivity matches structs.structs.EventAddressActivity.addressActivity.
// SQL handler: cache.handle_event_address_activity
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1493-1531)
//
// blockTime arrives as an RFC3339-style timestamp string from the chain;
// json.Unmarshal handles that directly into time.Time.
type AddressActivity struct {
	Address     string    `json:"address"`
	BlockHeight JSONInt   `json:"blockHeight"`
	BlockTime   time.Time `json:"blockTime"`
}
