package payload

// Player matches structs.structs.EventPlayer.player.
// SQL handler: cache.handle_event_player
// (cache-trigger-add-queue-20260427-ugc-fields.sql:23-125)
//
// Note: guildRank was added by the 20260325 handler but dropped by the
// 20260427 rewrite; we follow the final SQL and do not write player.guild_rank
// from the chain payload. structs.player.guild_rank still exists in the
// schema but is fed exclusively by the membership/permission flow.
type Player struct {
	ID             string  `json:"id"`
	Index          JSONInt `json:"index"`
	Creator        string  `json:"creator"`
	PrimaryAddress string  `json:"primaryAddress"`
	GuildID        string  `json:"guildId"`
	SubstationID   string  `json:"substationId"`
	PlanetID       string  `json:"planetId"`
	FleetID        string  `json:"fleetId"`
	Name           string  `json:"name"`
	PFP            string  `json:"pfp"`
	// PFPClientRenderAttributes is chain Player field 12
	// (pfpClientRenderAttributes), added in structsd v0.18.0: a compacted
	// JSON object string describing how the client renders the player's
	// locally generated profile picture. Written verbatim to
	// structs.player.pfp_client_render_attributes.
	PFPClientRenderAttributes string `json:"pfpClientRenderAttributes"`
}
