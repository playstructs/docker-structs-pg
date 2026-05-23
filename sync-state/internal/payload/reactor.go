package payload

// Reactor matches structs.structs.EventReactor.reactor.
// SQL handler: cache.handle_event_reactor
// (cache-trigger-add-queue-20260325-110b-schema-update.sql:237-293)
type Reactor struct {
	ID                string  `json:"id"`
	Validator         string  `json:"validator"`
	GuildID           string  `json:"guildId"`
	DefaultCommission Numeric `json:"defaultCommission"`
	Owner             string  `json:"owner"`
}
