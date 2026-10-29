package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestGuildSupplyIncludesGenesisEscrowAndOnlyAlphaCollateral(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		const guild, player = "0-995101", "1-995101"
		const holder, escrow, pool, pool2 = "supply-holder", "supply-protected-escrow", "supply-collateral", "supply-collateral-two"
		denom := "uguild." + guild
		seedIsolatedGuild(t, ctx, tx, guild, "supply-guild")
		seedIsolatedPlayer(t, ctx, tx, player, "supply-player", guild, "")
		if _, err := tx.Exec(ctx, `INSERT INTO structs.player_address(address,player_id,status) VALUES($1,$2,'approved')`, holder, player); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO structs.address_tag(address,label,entry) VALUES($1,'Type','Bank Collateral Pool'),($1,'GuildId',$2),($3,'Type','Bank Collateral Pool'),($3,'GuildId',$2)`, pool, guild, pool2); err != nil {
			t.Fatal(err)
		}
		seedIsolatedLedger(t, ctx, tx, holder, "100", "genesis", "credit", denom, 0)
		seedIsolatedLedger(t, ctx, tx, escrow, "50", "genesis", "credit", denom, 0)
		seedIsolatedLedger(t, ctx, tx, "supply-module", "20", "minted", "credit", denom, 1)
		seedIsolatedLedger(t, ctx, tx, "supply-module", "5", "burned", "debit", denom, 2)
		seedIsolatedLedger(t, ctx, tx, escrow, "10", "sent", "debit", denom, 3)
		seedIsolatedLedger(t, ctx, tx, "supply-unassociated", "10", "received", "credit", denom, 3)
		seedIsolatedLedger(t, ctx, tx, pool, "100", "received", "credit", "ualpha", 4)
		seedIsolatedLedger(t, ctx, tx, pool2, "100", "received", "credit", "ualpha", 4)
		seedIsolatedLedger(t, ctx, tx, pool, "9999", "received", "credit", "ibc/OTHER", 4)
		d := NewDirty()
		d.Guild(guild)
		d.Player(player)
		for _, address := range []string{holder, escrow, pool, pool2, "supply-module", "supply-unassociated"} {
			d.Address(address)
		}
		for i := 0; i < 2; i++ {
			if err := Recompute(ctx, tx, d, 4, time.Now().UTC(), InventoryFromLedger()); err != nil {
				t.Fatal(err)
			}
			var correct bool
			if err := tx.QueryRow(ctx, `SELECT supply=165 AND collateral=200 AND ratio=200::numeric/165
FROM structs.api_guild_bank WHERE guild_id=$1`, guild).Scan(&correct); err != nil || !correct {
				t.Fatalf("genesis supply/collateral wrong after pass %d: correct=%v err=%v", i, correct, err)
			}
			if err := tx.QueryRow(ctx, `SELECT minted_supply=165 AND collateral_balance=200 AND ratio=200::numeric/165
FROM view.guild_bank WHERE id=$1`, guild).Scan(&correct); err != nil || !correct {
				t.Fatalf("legacy bank view differs: correct=%v err=%v", correct, err)
			}
			if err := tx.QueryRow(ctx, `SELECT alpha_value=100*(200::numeric/165)
FROM structs.api_leaderboard_player WHERE player_id=$1`, player).Scan(&correct); err != nil || !correct {
				t.Fatalf("holder value differs: correct=%v err=%v", correct, err)
			}
		}
		var attributed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.asset_owner WHERE address=$1`, escrow).Scan(&attributed); err != nil || attributed != 0 {
			t.Fatalf("protected escrow incorrectly attributed: count=%d err=%v", attributed, err)
		}
	})
}
