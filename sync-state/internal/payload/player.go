package payload

// Player matches structs.structs.EventPlayer.player.
// SQL handler: cache.handle_event_player
// (cache-trigger-add-queue-20260427-ugc-fields.sql:23-125, plus
// guildRank restored from the 20260325 handler — the 20260427 rewrite
// silently dropped that field; nothing else writes player.guild_rank).
type Player struct {
	ID             string  `json:"id"`
	Index          JSONInt `json:"index"`
	Creator        string  `json:"creator"`
	PrimaryAddress string  `json:"primaryAddress"`
	GuildID        string  `json:"guildId"`
	SubstationID   string  `json:"substationId"`
	PlanetID       string  `json:"planetId"`
	FleetID        string  `json:"fleetId"`
	// GuildRank is chain Player field 9. Emitted as a quoted uint64
	// string by protojson, so JSONInt (not int64). Written to
	// structs.player.guild_rank; a rank of 0 is stored as 0 (matching
	// the 20260325 SQL handler).
	GuildRank JSONInt `json:"guildRank"`
	Name      string  `json:"name"`
	PFP       string  `json:"pfp"`
	// PFPClientRenderAttributes is chain Player field 12
	// (pfpClientRenderAttributes), added in structsd v0.18.0: a compacted
	// JSON object string describing how the client renders the player's
	// locally generated profile picture. Written verbatim to
	// structs.player.pfp_client_render_attributes.
	PFPClientRenderAttributes string `json:"pfpClientRenderAttributes"`
}
