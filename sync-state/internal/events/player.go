package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// playerHandler ports cache.handle_event_player
// (cache-trigger-add-queue-20260427-ugc-fields.sql:23-125).
//
// Writes structs.player (including chain UGC username/pfp) +
// structs.player_object(id, id).
//
// Note on guild_rank: the 20260325 handler wrote chain-provided guildRank
// to player.guild_rank, but the 20260427 rewrite dropped that field from
// the player payload entirely. We follow the final SQL — guild_rank stays
// fed by the membership/permission flow (Phase 3) instead of by the player
// event handler.
//
// Player is a downstream hub: player.guild_id flows to player_address.guild_id
// via PG triggers. The block-level event order ensures player events
// commit before planet / address events in the same block (we honor the
// chain's emit order, which already gets this right).
type playerHandler struct{}

func (playerHandler) CompositeKey() string {
	return "structs.structs.EventPlayer.player"
}

const playerUpsertSQL = `
INSERT INTO structs.player (
    id, index, creator, primary_address, guild_id,
    substation_id, planet_id, fleet_id,
    username, pfp,
    created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
   SET primary_address = EXCLUDED.primary_address,
       guild_id        = EXCLUDED.guild_id,
       substation_id   = EXCLUDED.substation_id,
       planet_id       = EXCLUDED.planet_id,
       fleet_id        = EXCLUDED.fleet_id,
       username        = EXCLUDED.username,
       pfp             = EXCLUDED.pfp,
       updated_at      = NOW()
 WHERE structs.player.primary_address IS DISTINCT FROM EXCLUDED.primary_address
    OR structs.player.guild_id        IS DISTINCT FROM EXCLUDED.guild_id
    OR structs.player.substation_id   IS DISTINCT FROM EXCLUDED.substation_id
    OR structs.player.planet_id       IS DISTINCT FROM EXCLUDED.planet_id
    OR structs.player.fleet_id        IS DISTINCT FROM EXCLUDED.fleet_id
    OR structs.player.username        IS DISTINCT FROM EXCLUDED.username
    OR structs.player.pfp             IS DISTINCT FROM EXCLUDED.pfp`

// playerPrevGuildSelectSQL fetches the player's existing guild_id so we
// can detect a change and propagate it to player_address.
const playerPrevGuildSelectSQL = `SELECT guild_id FROM structs.player WHERE id = $1`

// playerAddressGuildPropagateSQL ports cache.UDPATE_ADDRESS_GUILD (note
// the typo in the SQL function name; the trigger name is the canonical
// UPDATE_ADDRESS_GUILD_ID — cache-system.sql:1113-1126).
//
// SQL trigger is AFTER UPDATE only and gates on NEW.guild_id <> OLD.guild_id.
// We mirror that: only run when prev row existed AND guild_id changed.
// For first-INSERT case, address rows for this player will get the
// guild_id from their own EventAddress / EventAddressAssociation
// handlers; no propagation needed yet.
const playerAddressGuildPropagateSQL = `
UPDATE structs.player_address
   SET guild_id = $2
 WHERE player_id = $1
   AND guild_id IS DISTINCT FROM $2`

func (playerHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Player](raw)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("player: empty id")
	}

	var (
		prevExists   bool
		prevGuildPtr *string
	)
	err = tx.QueryRow(ctx, playerPrevGuildSelectSQL, p.ID).Scan(&prevGuildPtr)
	switch {
	case err == nil:
		prevExists = true
	case errors.Is(err, pgx.ErrNoRows):
		// fresh player — UPDATE_ADDRESS_GUILD_ID never fired on
		// INSERT in the dropped SQL trigger; we match that semantics.
	default:
		return fmt.Errorf("player prev guild id=%s: %w", p.ID, err)
	}

	if _, err := tx.Exec(ctx, playerUpsertSQL,
		p.ID,
		p.Index.Int64(),
		payload.NullableText(p.Creator),
		payload.NullableText(p.PrimaryAddress),
		payload.NullableText(p.GuildID),
		payload.NullableText(p.SubstationID),
		payload.NullableText(p.PlanetID),
		payload.NullableText(p.FleetID),
		payload.NullableText(p.Name),
		payload.NullableText(p.PFP),
	); err != nil {
		return fmt.Errorf("player upsert id=%s: %w", p.ID, err)
	}
	if err := upsertPlayerObject(ctx, tx, p.ID, p.ID); err != nil {
		return err
	}

	if prevExists {
		prevGuild := derefStr(prevGuildPtr)
		if prevGuild != p.GuildID {
			if _, err := tx.Exec(ctx, playerAddressGuildPropagateSQL,
				p.ID,
				payload.NullableText(p.GuildID),
			); err != nil {
				return fmt.Errorf("player_address guild propagate player=%s prev=%q new=%q: %w",
					p.ID, prevGuild, p.GuildID, err)
			}
		}
	}
	return nil
}
