package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// guildRankPermissionHandler ports cache.handle_event_guild_rank_permission
// (cache-trigger-add-queue-20260325-110b-schema-update.sql:295-343).
//
// Two branches:
//   - rank == 0 → DELETE the (object_id, guild_id, permission) row
//   - else      → UPSERT with the new rank
//
// IS DISTINCT FROM guard on `rank` on the UPSERT branch suppresses
// no-op updates.
type guildRankPermissionHandler struct{}

func (guildRankPermissionHandler) CompositeKey() string {
	return "structs.structs.EventGuildRankPermission.guildRankPermissionRecord"
}

const guildRankPermissionDeleteSQL = `
DELETE FROM structs.permission_guild_rank
 WHERE object_id  = $1
   AND guild_id   = $2
   AND permission = $3`

const guildRankPermissionUpsertSQL = `
INSERT INTO structs.permission_guild_rank (
    object_id, guild_id, permission, rank, updated_at
) VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (object_id, guild_id, permission) DO UPDATE
   SET rank       = EXCLUDED.rank,
       updated_at = EXCLUDED.updated_at
 WHERE structs.permission_guild_rank.rank IS DISTINCT FROM EXCLUDED.rank`

func (guildRankPermissionHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.GuildRankPermission](raw)
	if err != nil {
		return err
	}
	if p.ObjectID == "" || p.GuildID == "" {
		return fmt.Errorf("guild_rank_permission: empty object_id (%q) or guild_id (%q)", p.ObjectID, p.GuildID)
	}
	if p.Rank.Int64() == 0 {
		if _, err := tx.Exec(ctx, guildRankPermissionDeleteSQL,
			p.ObjectID, p.GuildID, p.Permissions.Int64(),
		); err != nil {
			return fmt.Errorf("guild_rank_permission delete (%s,%s,%d): %w",
				p.ObjectID, p.GuildID, p.Permissions.Int64(), err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, guildRankPermissionUpsertSQL,
		p.ObjectID, p.GuildID, p.Permissions.Int64(), p.Rank.Int64(),
	); err != nil {
		return fmt.Errorf("guild_rank_permission upsert (%s,%s,%d): %w",
			p.ObjectID, p.GuildID, p.Permissions.Int64(), err)
	}
	return nil
}
