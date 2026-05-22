package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// guildHandler ports cache.handle_event_guild
// (cache-trigger-add-queue-20260427-ugc-fields.sql:132-247).
// UPSERT structs.guild + structs.player_object + structs.guild_meta UGC.
//
// Note: join_infusion_minimum is GENERATED from join_infusion_minimum_p;
// we only ever write the _p column. join_infusion_minimum_bypass_*
// columns are text booleans on the chain side (handler-stage encoding).
type guildHandler struct{}

func (guildHandler) CompositeKey() string {
	return "structs.structs.EventGuild.guild"
}

const guildUpsertSQL = `
INSERT INTO structs.guild (
    id, index, endpoint,
    join_infusion_minimum_p,
    join_infusion_minimum_bypass_by_request,
    join_infusion_minimum_bypass_by_invite,
    primary_reactor_id, entry_substation_id, entry_rank,
    creator, owner, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET endpoint                                = EXCLUDED.endpoint,
       join_infusion_minimum_p                 = EXCLUDED.join_infusion_minimum_p,
       join_infusion_minimum_bypass_by_request = EXCLUDED.join_infusion_minimum_bypass_by_request,
       join_infusion_minimum_bypass_by_invite  = EXCLUDED.join_infusion_minimum_bypass_by_invite,
       primary_reactor_id                      = EXCLUDED.primary_reactor_id,
       entry_substation_id                     = EXCLUDED.entry_substation_id,
       entry_rank                              = EXCLUDED.entry_rank,
       owner                                   = EXCLUDED.owner,
       updated_at                              = NOW()
 WHERE structs.guild.endpoint                                IS DISTINCT FROM EXCLUDED.endpoint
    OR structs.guild.join_infusion_minimum_p                 IS DISTINCT FROM EXCLUDED.join_infusion_minimum_p
    OR structs.guild.join_infusion_minimum_bypass_by_request IS DISTINCT FROM EXCLUDED.join_infusion_minimum_bypass_by_request
    OR structs.guild.join_infusion_minimum_bypass_by_invite  IS DISTINCT FROM EXCLUDED.join_infusion_minimum_bypass_by_invite
    OR structs.guild.primary_reactor_id                      IS DISTINCT FROM EXCLUDED.primary_reactor_id
    OR structs.guild.entry_substation_id                     IS DISTINCT FROM EXCLUDED.entry_substation_id
    OR structs.guild.entry_rank                              IS DISTINCT FROM EXCLUDED.entry_rank
    OR structs.guild.owner                                   IS DISTINCT FROM EXCLUDED.owner`

const guildMetaUpsertSQL = `
INSERT INTO structs.guild_meta (id, name, pfp, created_at, updated_at)
VALUES ($1, $2, $3, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET name       = EXCLUDED.name,
       pfp        = EXCLUDED.pfp,
       updated_at = NOW()
 WHERE structs.guild_meta.name IS DISTINCT FROM EXCLUDED.name
    OR structs.guild_meta.pfp  IS DISTINCT FROM EXCLUDED.pfp`

func (guildHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Guild](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("guild: empty id")
	}
	if _, err := tx.Exec(ctx, guildUpsertSQL,
		p.ID,
		p.Index.Int64(),
		payload.NullableText(p.Endpoint),
		p.JoinInfusionMinimum.Int64(),
		payload.NullableText(p.JoinInfusionMinimumBypassByRequest),
		payload.NullableText(p.JoinInfusionMinimumBypassByInvite),
		payload.NullableText(p.PrimaryReactorID),
		payload.NullableText(p.EntrySubstationID),
		p.EntryRank.Int64(),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Owner),
	); err != nil {
		return fmt.Errorf("guild upsert id=%s: %w", p.ID, err)
	}
	if err := upsertPlayerObject(ctx, tx, p.ID, p.Owner); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, guildMetaUpsertSQL,
		p.ID,
		payload.NullableText(p.Name),
		payload.NullableText(p.PFP),
	); err != nil {
		return fmt.Errorf("guild_meta upsert id=%s: %w", p.ID, err)
	}
	return nil
}
