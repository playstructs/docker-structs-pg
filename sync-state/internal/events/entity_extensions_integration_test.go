package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/protocol"
)

func TestEntityExtensions_RanksAndReplacement(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx, bc := context.Background(), bctx()
		handle(t, ctx, tx, playerHandler{}, bc, json.RawMessage(`{"id":"1-990993","index":"990993","primaryAddress":"structs1ranktest","guildRank":"18446744073709551615"}`))
		handle(t, ctx, tx, guildRankPermissionHandler{}, bc, json.RawMessage(`{"objectId":"0-990993","guildId":"0-990993","permissions":"1","rank":"18446744073709551615"}`))
		handle(t, ctx, tx, guildHandler{}, bc, json.RawMessage(`{"id":"0-990993","index":"990993","owner":"1-990993","entryRank":"18446744073709551615"}`))
		bc.ChainID, bc.Height = "structstestnet-111", protocol.V022TestnetHeight
		g := map[string]any{"id": "0-990993", "index": "990993", "owner": "1-990993", "entryRank": "18446744073709551615",
			"rankPurchaseBasePrice": "18446744073709551615", "rankPurchaseStepIncrease": "1", "rankPurchaseMinimumRank": "1",
			"joinByRankPurchaseEnabled": true, "bankConvertInFee": "0.1", "bankConvertOutFee": "0", "charterSolverId": "1-990993"}
		handle(t, ctx, tx, guildHandler{}, bc, mustJSON(t, g))
		var rank string
		for _, query := range []string{
			`SELECT guild_rank::text FROM structs.player WHERE id='1-990993'`,
			`SELECT entry_rank::text FROM structs.guild WHERE id='0-990993'`,
			`SELECT rank::text FROM structs.permission_guild_rank WHERE object_id='0-990993'`,
		} {
			if err := tx.QueryRow(ctx, query).Scan(&rank); err != nil || rank != "18446744073709551615" {
				t.Fatalf("unsigned rank=%s err=%v", rank, err)
			}
		}
		g["rankPurchaseBasePrice"], g["rankPurchaseStepIncrease"], g["rankPurchaseMinimumRank"], g["joinByRankPurchaseEnabled"] = "0", "0", "0", false
		g["owner"] = "1-990992"
		handle(t, ctx, tx, guildHandler{}, bc, mustJSON(t, g))
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT rank_purchase_base_price::text,join_by_rank_purchase_enabled FROM structs.guild WHERE id='0-990993'`).Scan(&rank, &enabled); err != nil || rank != "0" || enabled {
			t.Fatalf("zero/false replacement=%s %v %v", rank, enabled, err)
		}
		if _, ok := bc.Dirty.Players["1-990992"]; !ok {
			t.Fatal("new guild controller not invalidated")
		}
		if _, ok := bc.Dirty.Players["1-990993"]; !ok {
			t.Fatal("former guild controller not invalidated")
		}
		handle(t, ctx, tx, substationHandler{}, bc, json.RawMessage(`{"id":"4-990993","owner":"1-990993","deletion_pending":true,"deletion_migration_substation_id":"4-990992"}`))
		handle(t, ctx, tx, substationHandler{}, bc, json.RawMessage(`{"id":"4-990993","owner":"1-990993","deletion_pending":false,"deletion_migration_substation_id":""}`))
		var destination string
		if err := tx.QueryRow(ctx, `SELECT deletion_pending,deletion_migration_substation_id FROM structs.substation WHERE id='4-990993'`).Scan(&enabled, &destination); err != nil || enabled || destination != "" {
			t.Fatalf("substation replacement=%v %s %v", enabled, destination, err)
		}
	})
}
