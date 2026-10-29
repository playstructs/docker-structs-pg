package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// guildHandler ports cache.handle_event_guild
// (cache-trigger-add-queue-20260427-ugc-fields.sql:132-247).
// UPSERT structs.guild (including chain UGC name/pfp) + structs.player_object.
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
    creator, owner, name, pfp,
    bank_convert_in_fee, bank_convert_out_fee, charter_solver_id,
    rank_purchase_base_price, rank_purchase_step_increase, rank_purchase_minimum_rank, join_by_rank_purchase_enabled,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET endpoint                                = EXCLUDED.endpoint,
       join_infusion_minimum_p                 = EXCLUDED.join_infusion_minimum_p,
       join_infusion_minimum_bypass_by_request = EXCLUDED.join_infusion_minimum_bypass_by_request,
       join_infusion_minimum_bypass_by_invite  = EXCLUDED.join_infusion_minimum_bypass_by_invite,
       primary_reactor_id                      = EXCLUDED.primary_reactor_id,
       entry_substation_id                     = EXCLUDED.entry_substation_id,
       entry_rank                              = EXCLUDED.entry_rank,
       owner                                   = EXCLUDED.owner,
       name                                    = EXCLUDED.name,
       pfp                                     = EXCLUDED.pfp,
       bank_convert_in_fee                     = EXCLUDED.bank_convert_in_fee,
       bank_convert_out_fee                    = EXCLUDED.bank_convert_out_fee,
       charter_solver_id                       = EXCLUDED.charter_solver_id,
       rank_purchase_base_price                = EXCLUDED.rank_purchase_base_price,
       rank_purchase_step_increase             = EXCLUDED.rank_purchase_step_increase,
       rank_purchase_minimum_rank              = EXCLUDED.rank_purchase_minimum_rank,
       join_by_rank_purchase_enabled           = EXCLUDED.join_by_rank_purchase_enabled,
       updated_at                              = NOW()
 WHERE structs.guild.endpoint                                IS DISTINCT FROM EXCLUDED.endpoint
    OR structs.guild.join_infusion_minimum_p                 IS DISTINCT FROM EXCLUDED.join_infusion_minimum_p
    OR structs.guild.join_infusion_minimum_bypass_by_request IS DISTINCT FROM EXCLUDED.join_infusion_minimum_bypass_by_request
    OR structs.guild.join_infusion_minimum_bypass_by_invite  IS DISTINCT FROM EXCLUDED.join_infusion_minimum_bypass_by_invite
    OR structs.guild.primary_reactor_id                      IS DISTINCT FROM EXCLUDED.primary_reactor_id
    OR structs.guild.entry_substation_id                     IS DISTINCT FROM EXCLUDED.entry_substation_id
    OR structs.guild.entry_rank                              IS DISTINCT FROM EXCLUDED.entry_rank
    OR structs.guild.owner                                   IS DISTINCT FROM EXCLUDED.owner
    OR structs.guild.name                                    IS DISTINCT FROM EXCLUDED.name
    OR structs.guild.pfp                                     IS DISTINCT FROM EXCLUDED.pfp
    OR structs.guild.bank_convert_in_fee                     IS DISTINCT FROM EXCLUDED.bank_convert_in_fee
    OR structs.guild.bank_convert_out_fee                    IS DISTINCT FROM EXCLUDED.bank_convert_out_fee
    OR structs.guild.charter_solver_id                       IS DISTINCT FROM EXCLUDED.charter_solver_id
    OR structs.guild.rank_purchase_base_price                IS DISTINCT FROM EXCLUDED.rank_purchase_base_price
    OR structs.guild.rank_purchase_step_increase             IS DISTINCT FROM EXCLUDED.rank_purchase_step_increase
    OR structs.guild.rank_purchase_minimum_rank              IS DISTINCT FROM EXCLUDED.rank_purchase_minimum_rank
    OR structs.guild.join_by_rank_purchase_enabled           IS DISTINCT FROM EXCLUDED.join_by_rank_purchase_enabled`

func (guildHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeVersioned[payload.Guild](raw, bctx.V022Active(),
		"rankPurchaseBasePrice", "rankPurchaseStepIncrease", "rankPurchaseMinimumRank", "joinByRankPurchaseEnabled")
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("guild: empty id")
	}
	var previousOwner *string
	if err := tx.QueryRow(ctx, `SELECT owner FROM structs.guild WHERE id=$1`, p.ID).Scan(&previousOwner); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, guildUpsertSQL,
		p.ID,
		p.Index.Int64(),
		payload.NullableText(p.Endpoint),
		p.JoinInfusionMinimum.PgValue(),
		payload.NullableText(p.JoinInfusionMinimumBypassByRequest),
		payload.NullableText(p.JoinInfusionMinimumBypassByInvite),
		payload.NullableText(p.PrimaryReactorID),
		payload.NullableText(p.EntrySubstationID),
		p.EntryRank.PgValue(),
		payload.NullableText(p.Creator),
		payload.NullableText(p.Owner),
		payload.NullableText(p.Name),
		payload.NullableText(p.PFP),
		p.BankConvertInFee.PgValue(),
		p.BankConvertOutFee.PgValue(),
		payload.NullableText(p.CharterSolverID),
		p.RankPurchaseBasePrice.PgValue(),
		p.RankPurchaseStepIncrease.PgValue(),
		p.RankPurchaseMinimumRank.PgValue(),
		p.JoinByRankPurchaseEnabled,
	); err != nil {
		return fmt.Errorf("guild upsert id=%s: %w", p.ID, err)
	}
	if err := upsertPlayerObject(ctx, tx, p.ID, p.Owner); err != nil {
		return err
	}
	bctx.Dirty.Guild(p.ID)
	if previousOwner != nil && *previousOwner != p.Owner {
		bctx.Dirty.Player(*previousOwner)
	}
	bctx.Dirty.Player(p.Owner)
	return nil
}
