package payload

// GuildMembershipApplication matches
// structs.structs.EventGuildMembershipApplication.guildMembershipApplication.
// SQL handler: cache.handle_event_guild_membership_application
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1434-1491)
type GuildMembershipApplication struct {
	GuildID            string `json:"guildId"`
	PlayerID           string `json:"playerId"`
	JoinType           string `json:"joinType"`
	RegistrationStatus string `json:"registrationStatus"`
	Proposer           string `json:"proposer"`
	SubstationID       string `json:"substationId"`
}
