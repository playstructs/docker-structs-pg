package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// guildMembershipApplicationHandler ports
// cache.handle_event_guild_membership_application
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1434-1491).
//
// Single UPSERT on (guild_id, player_id). PG trigger GUILD_MEMBERSHIP_NOTIFY
// fires on every INSERT/UPDATE so the webapp gets a SSE event — that
// stays in PG (GRASS triggers are explicitly NOT migrated to Go).
type guildMembershipApplicationHandler struct{}

func (guildMembershipApplicationHandler) CompositeKey() string {
	return "structs.structs.EventGuildMembershipApplication.guildMembershipApplication"
}

const guildMembershipApplicationUpsertSQL = `
INSERT INTO structs.guild_membership_application (
    guild_id, player_id, join_type, status, proposer, substation_id,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
ON CONFLICT (guild_id, player_id) DO UPDATE
   SET join_type     = EXCLUDED.join_type,
       status        = EXCLUDED.status,
       proposer      = EXCLUDED.proposer,
       substation_id = EXCLUDED.substation_id,
       updated_at    = NOW()
 WHERE structs.guild_membership_application.join_type     IS DISTINCT FROM EXCLUDED.join_type
    OR structs.guild_membership_application.status        IS DISTINCT FROM EXCLUDED.status
    OR structs.guild_membership_application.proposer      IS DISTINCT FROM EXCLUDED.proposer
    OR structs.guild_membership_application.substation_id IS DISTINCT FROM EXCLUDED.substation_id`

func (guildMembershipApplicationHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.GuildMembershipApplication](raw)
	if err != nil {
		return err
	}
	if p.GuildID == "" || p.PlayerID == "" {
		return fmt.Errorf("guild_membership_application: empty guild_id (%q) or player_id (%q)", p.GuildID, p.PlayerID)
	}
	if _, err := tx.Exec(ctx, guildMembershipApplicationUpsertSQL,
		p.GuildID,
		p.PlayerID,
		payload.NullableText(p.JoinType),
		payload.NullableText(p.RegistrationStatus),
		payload.NullableText(p.Proposer),
		payload.NullableText(p.SubstationID),
	); err != nil {
		return fmt.Errorf("guild_membership_application upsert (%s,%s): %w", p.GuildID, p.PlayerID, err)
	}
	return nil
}
