package payload

// GuildRankPermission matches
// structs.structs.EventGuildRankPermission.guildRankPermissionRecord.
// SQL handler: cache.handle_event_guild_rank_permission
// (cache-trigger-add-queue-20260325-110b-schema-update.sql:295-343)
//
// rank == 0 means DELETE the (objectId, guildId, permission) row;
// otherwise UPSERT with rank.
type GuildRankPermission struct {
	ObjectID    string  `json:"objectId"`
	GuildID     string  `json:"guildId"`
	Permissions JSONInt `json:"permissions"`
	Rank        JSONInt `json:"rank"`
}
