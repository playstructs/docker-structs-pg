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

func TestLeaderboardMetricFiltersAndRepeatedPlanning(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		const guild, otherGuild, emptyGuild = "0-992401", "0-992402", "0-992403"
		const sub, emptySub = "4-992401", "4-992402"
		for _, id := range []string{guild, otherGuild, emptyGuild} {
			seedIsolatedGuild(t, ctx, tx, id, "metric-filter")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO structs.substation(id,owner) VALUES ($1,'1-992401'),($2,'1-992401')`, sub, emptySub); err != nil {
			t.Fatal(err)
		}
		seedIsolatedPlayer(t, ctx, tx, "1-992401", "metric-one", guild, sub)
		seedIsolatedPlayer(t, ctx, tx, "1-992402", "metric-only-ore", guild, sub)
		seedIsolatedPlayer(t, ctx, tx, "1-992403", "metric-no-grid", otherGuild, sub)
		seedIsolatedPlayer(t, ctx, tx, "1-992404", "metric-no-substation", guild, "")
		if _, err := tx.Exec(ctx, `INSERT INTO structs.grid(id,object_id,object_type,attribute_type,val) VALUES
('2-1-992401','1-992401','player','capacity',13),
('3-1-992401','1-992401','player','load',7),
('7-1-992401','1-992401','player','structsLoad',11),
('0-1-992401','1-992401','player','ore',999999),
('0-1-992402','1-992402','player','ore',999999),
('2-1-992404','1-992404','player','capacity',1000000000000000000009),
('3-4-992401','4-992401','substation','load',99),
('6-4-992401','4-992401','substation','connectionCapacity',500),
('2-4-992401','4-992401','substation','capacity',999999)`); err != nil {
			t.Fatal(err)
		}
		var mode string
		if err := tx.QueryRow(ctx, `SHOW plan_cache_mode`).Scan(&mode); err != nil || mode != "auto" {
			t.Fatalf("fixture requires normal planner mode: %s %v", mode, err)
		}
		if tx.Conn().Config().DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
			t.Fatal("fixture requires ordinary statement caching for other queries")
		}
		// Cross the planner's first-five-executions boundary with varying ID
		// arrays. The aggregates must not settle on a generic named plan.
		for i := 0; i < 12; i++ {
			d := NewDirty()
			d.Guild(guild)
			d.Substation(sub)
			if i%2 == 0 {
				d.Guild(otherGuild)
				d.Guild(emptyGuild)
				d.Substation(emptySub)
			}
			if err := recomputeGuilds(ctx, tx, d); err != nil {
				t.Fatal(err)
			}
			if err := recomputeSubstations(ctx, tx, d); err != nil {
				t.Fatal(err)
			}
		}
		for _, want := range []struct {
			id, capacity, load, shared string
			players                    int
		}{
			{guild, "1000000000000000000022", "18", "500", 3},
			{otherGuild, "0", "0", "500", 1},
			{emptyGuild, "0", "0", "0", 0},
		} {
			var capacity, load, shared string
			var players int
			if err := tx.QueryRow(ctx, `SELECT member_capacity::text,member_load::text,shared_connection_capacity::text,player_count
FROM structs.api_leaderboard_guild WHERE guild_id=$1`, want.id).Scan(&capacity, &load, &shared, &players); err != nil {
				t.Fatal(err)
			}
			if capacity != want.capacity || load != want.load || shared != want.shared || players != want.players {
				t.Fatalf("guild %s: capacity=%s load=%s shared=%s players=%d", want.id, capacity, load, shared, players)
			}
		}
		for _, want := range []struct {
			id, capacity, load, shared string
			players                    int
		}{
			{sub, "13", "99", "500", 3},
			{emptySub, "0", "0", "0", 0},
		} {
			var capacity, load, shared string
			var players int
			if err := tx.QueryRow(ctx, `SELECT member_capacity::text,load::text,shared_connection_capacity::text,player_count
FROM structs.api_leaderboard_substation WHERE substation_id=$1`, want.id).Scan(&capacity, &load, &shared, &players); err != nil {
				t.Fatal(err)
			}
			if capacity != want.capacity || load != want.load || shared != want.shared || players != want.players {
				t.Fatalf("substation %s: capacity=%s load=%s shared=%s players=%d", want.id, capacity, load, shared, players)
			}
		}
		var cachedAggregates, cachedDeletes int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements
WHERE statement LIKE $1`, "%WITH player_metric AS%").Scan(&cachedAggregates); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements
WHERE statement LIKE 'DELETE FROM structs.api_leaderboard_%'`).Scan(&cachedDeletes); err != nil {
			t.Fatal(err)
		}
		if cachedAggregates != 0 || cachedDeletes != 2 {
			t.Fatalf("aggregate caching=%d unrelated delete caching=%d", cachedAggregates, cachedDeletes)
		}
		if err := tx.QueryRow(ctx, `SHOW plan_cache_mode`).Scan(&mode); err != nil || mode != "auto" {
			t.Fatalf("refresh changed planner mode: %s %v", mode, err)
		}
	})
}
