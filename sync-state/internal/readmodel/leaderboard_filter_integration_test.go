package readmodel

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestFilteredLeaderboardsKeepWholeGroups(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		g1, g2, sub := "0-992301", "0-992302", "4-992301"
		seedIsolatedGuild(t, ctx, tx, g1, "filter-one")
		seedIsolatedGuild(t, ctx, tx, g2, "filter-two")
		if _, err := tx.Exec(ctx, `INSERT INTO structs.substation(id,owner) VALUES ($1,'1-992301')`, sub); err != nil {
			t.Fatal(err)
		}
		for i, p := range []string{"1-992301", "1-992302", "1-992303"} {
			guild := g1
			if i == 2 {
				guild = g2
			}
			seedIsolatedPlayer(t, ctx, tx, p, "filter"+p, guild, sub)
			if _, err := tx.Exec(ctx, `INSERT INTO structs.grid(id,attribute_type,object_type,object_index,object_id,val)
   VALUES ('2-'||$1,'capacity','player',$2,$1,$3)`, p, 992301+i, (i+1)*10); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO structs.grid(id,attribute_type,object_type,object_index,object_id,val)
  VALUES ('6-4-992301','connectionCapacity','substation',992301,$1,500)`, sub); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL plan_cache_mode=force_generic_plan`); err != nil {
			t.Fatal(err)
		}
		d := NewDirty()
		d.Guild(g1)
		d.Guild(g2)
		d.Substation(sub)
		checkGuild := func(id string, count int, capacity string) {
			t.Helper()
			var n int
			var cap, shared string
			if err := tx.QueryRow(ctx, `SELECT player_count,member_capacity::text,shared_connection_capacity::text FROM structs.api_leaderboard_guild WHERE guild_id=$1`, id).Scan(&n, &cap, &shared); err != nil {
				t.Fatal(err)
			}
			wantShared := "500"
			if count == 0 {
				wantShared = "0"
			}
			if n != count || cap != capacity || shared != wantShared {
				t.Fatalf("guild %s: %d %s %s", id, n, cap, shared)
			}
		}
		checkSub := func(count int, capacity string) {
			t.Helper()
			var n int
			var cap, shared string
			if err := tx.QueryRow(ctx, `SELECT player_count,member_capacity::text,shared_connection_capacity::text FROM structs.api_leaderboard_substation WHERE substation_id=$1`, sub).Scan(&n, &cap, &shared); err != nil {
				t.Fatal(err)
			}
			if n != count || cap != capacity || shared != "500" {
				t.Fatalf("substation: %d %s %s", n, cap, shared)
			}
		}
		refresh := func() {
			t.Helper()
			if err := recomputeGuilds(ctx, tx, d); err != nil {
				t.Fatal(err)
			}
			if err := recomputeSubstations(ctx, tx, d); err != nil {
				t.Fatal(err)
			}
		}
		refresh()
		checkGuild(g1, 2, "30")
		checkGuild(g2, 1, "30")
		checkSub(3, "60")
		// One dirty player still requires all members, including another guild's
		// member of the same substation. Unrequested guild rows stay untouched.
		d = NewDirty()
		d.Player("1-992301")
		d.Guild(g1)
		d.Substation(sub)
		if _, err := tx.Exec(ctx, `UPDATE structs.grid SET val=11 WHERE id='2-1-992301'`); err != nil {
			t.Fatal(err)
		}
		refresh()
		checkGuild(g1, 2, "31")
		checkGuild(g2, 1, "30")
		checkSub(3, "61")
		// Move members out: affected old groups must become empty, with no stale sums.
		if _, err := tx.Exec(ctx, `UPDATE structs.player SET guild_id=$2,substation_id=NULL WHERE guild_id=$1`, g1, g2); err != nil {
			t.Fatal(err)
		}
		d.Guild(g2)
		refresh()
		checkGuild(g1, 0, "0")
		checkGuild(g2, 3, "61")
		checkSub(1, "30")
	})
}
