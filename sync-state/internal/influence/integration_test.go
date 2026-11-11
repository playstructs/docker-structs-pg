package influence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/rpc"
)

func fixture(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	dsn := os.Getenv("INFLUENCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INFLUENCE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	c, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := c.Begin(ctx)
	if e != nil {
		c.Close(ctx)
		t.Fatal(e)
	}
	t.Cleanup(func() { tx.Rollback(ctx); c.Close(ctx) })
	_, e = tx.Exec(ctx, `INSERT INTO structs.player(id,guild_id) VALUES('1-a','0-a'),('1-b','0-a'),('1-d','0-d');
 INSERT INTO structs.fleet(id,owner) VALUES('9-a','1-a'),('9-b','1-b');
 INSERT INTO structs.planet(id,owner,sector) VALUES('2-a','1-d',127),('2-b','1-d',999)`)
	if e != nil {
		t.Fatal(e)
	}
	if e = ValidateSchema(ctx, tx); e != nil {
		t.Fatal(e)
	}
	return ctx, tx
}

func TestIntegrationGuildlessAndIndexerRole(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 5)
	if _, e := tx.Exec(c, `SET LOCAL ROLE structs_indexer`); e != nil {
		t.Fatal(e)
	}
	raid(t, c, tx, tr, "a", "2-a", 0)
	raid(t, c, tx, tr, "a", "2-a", 1)
	finish(t, c, tx, tr)
	tr = start(t, c, tx, 2)
	move(t, c, tx, tr, "1-a", "")
	finish(t, c, tx, tr)
	if _, e := tx.Exec(c, `RESET ROLE`); e != nil {
		t.Fatal(e)
	}
	if points(t, c, tx, id, "0-a") != 0 {
		t.Fatal("guildless points remained")
	}
	tr = start(t, c, tx, 3)
	move(t, c, tx, tr, "1-a", "0-b")
	finish(t, c, tx, tr)
	if points(t, c, tx, id, "0-b") != 300 {
		t.Fatal("portable guildless achievements lost or duplicated")
	}
	reconcile(t, c, tx, id)
	var allowed bool
	if e := tx.QueryRow(c, `SELECT has_table_privilege('structs_indexer','structs.influence_raid','UPDATE') OR has_table_privilege('structs_indexer','structs.influence_membership_history','DELETE')`).Scan(&allowed); e != nil || allowed {
		t.Fatalf("immutable source privileges: %v %v", allowed, e)
	}
}

func TestIntegrationGuardsAndRollback(t *testing.T) {
	c, tx := fixture(t)
	child, e := tx.Begin(c)
	if e != nil {
		t.Fatal(e)
	}
	tr := start(t, c, child, 1)
	create(t, c, child, 1, 4)
	raid(t, c, child, tr, "a", "2-a", 0)
	finish(t, c, child, tr)
	if e = child.Rollback(c); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = tx.QueryRow(c, `SELECT count(*) FROM structs.influence_raid`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("rollback raid %d %v", n, e)
	}
	tr = start(t, c, tx, 1)
	id := create(t, c, tx, 1, 3)
	raid(t, c, tx, tr, "a", "2-a", 0)
	// Identical source replay does not double apply; a conflicting replay fails.
	if _, e = tx.Exec(c, `SELECT structs.influence_record_raid(jsonb_build_object('chain',chain_id,'height',height,'phase',phase,'tx_index',tx_index,'event_index',event_index,'hash',block_hash,'time',block_time,'msg_index',msg_index),source) FROM structs.influence_raid`); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{
		`SELECT structs.influence_record_raid(jsonb_build_object('chain',chain_id,'height',height,'phase',phase,'tx_index',tx_index,'event_index',event_index,'hash',block_hash,'time',block_time,'msg_index',msg_index),source||'{"sector":999}'::jsonb) FROM structs.influence_raid`,
		`UPDATE structs.influence_rule_set SET win_points=123 WHERE rule_id=1`,
		`UPDATE structs.influence_raid SET sector=999`,
		`DELETE FROM structs.influence_membership_history`,
		`SELECT structs.influence_create_contest('influence-test',0,3)`} {
		child, e = tx.Begin(c)
		if e != nil {
			t.Fatal(e)
		}
		_, e = child.Exec(c, q)
		child.Rollback(c)
		if e == nil {
			t.Fatalf("accepted invalid mutation: %s", q)
		}
	}
	finish(t, c, tx, tr)
	for _, h := range []int64{1, 3} {
		child, e = tx.Begin(c)
		if e != nil {
			t.Fatal(e)
		}
		_, e = Begin(c, child, "influence-test", h, "wrong", time.Now(), false)
		child.Rollback(c)
		if e == nil {
			t.Fatal("accepted gap/hash conflict")
		}
	}
	if points(t, c, tx, id, "0-a") != 300 {
		t.Fatal("replay double-applied")
	}
}

func TestIntegrationBootstrapBoundary(t *testing.T) {
	c, tx := fixture(t)
	if _, e := tx.Exec(c, `INSERT INTO structs.current_block(chain,height) VALUES('influence-test',50)`); e != nil {
		t.Fatal(e)
	}
	tr, e := Begin(c, tx, "influence-test", 50, "hash-50", time.Now(), true)
	if e != nil {
		t.Fatal(e)
	}
	if tr.enabled {
		t.Fatal("invented boundary history")
	}
	child, e := tx.Begin(c)
	if e != nil {
		t.Fatal(e)
	}
	_, e = Begin(c, child, "influence-test", 51, "hash-51", time.Now(), true)
	child.Rollback(c)
	if e == nil {
		t.Fatal("used already-current base state to fill missing historical coverage")
	}
	tr = start(t, c, tx, 51)
	id := create(t, c, tx, 51, 51)
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	if points(t, c, tx, id, "0-a") != 300 {
		t.Fatal("boundary failed")
	}
}

// Explicitly opt in: this test commits in a dedicated disposable database to
// exercise Timescale's top-level refresh CALL and the crash-safe watermark.
func TestIntegrationRollup(t *testing.T) {
	if os.Getenv("INFLUENCE_TEST_MAINTENANCE") != "1" {
		t.Skip("requires disposable maintenance database")
	}
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 1999)
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	if _, e := tx.Exec(c, `DO $$ BEGIN FOR h IN 2..999 LOOP PERFORM structs.influence_finish_block('influence-test',h,'hash-'||h); END LOOP; END $$`); e != nil {
		t.Fatal(e)
	}
	if e := tx.Commit(c); e != nil {
		t.Fatal(e)
	}
	conn, e := pgx.Connect(c, os.Getenv("INFLUENCE_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(c)
	defer func() {
		cleanup, e := conn.Begin(c)
		if e != nil {
			t.Error(e)
			return
		}
		defer cleanup.Rollback(c)
		if _, e = cleanup.Exec(c, `SET LOCAL structs.influence_recovery='on'`); e != nil {
			t.Error(e)
			return
		}
		for _, table := range []string{"published_build", "rollup_coverage", "checkpoint", "score_change", "guild_sector", "player_sector", "planet_claim", "guild_planet_win", "player_planet_win", "contest_planet", "member"} {
			_, e = cleanup.Exec(c, `DELETE FROM structs.influence_`+table+` WHERE build_id IN (SELECT build_id FROM structs.influence_projection_build WHERE chain_id='influence-test')`)
			if e != nil {
				t.Error(e)
				return
			}
		}
		for _, table := range []string{"projection_build", "contest", "raid", "membership_history", "source_cursor"} {
			if _, e = cleanup.Exec(c, `DELETE FROM structs.influence_`+table+` WHERE chain_id='influence-test'`); e != nil {
				t.Error(e)
				return
			}
		}
		if _, e = cleanup.Exec(c, `DELETE FROM structs.planet WHERE id IN ('2-a','2-b'); DELETE FROM structs.fleet WHERE id IN ('9-a','9-b'); DELETE FROM structs.player WHERE id IN ('1-a','1-b','1-d')`); e != nil {
			t.Error(e)
			return
		}
		if e = cleanup.Commit(c); e != nil {
			t.Error(e)
		}
	}()
	if e = Refresh(c, conn, io.Discard); e != nil {
		t.Fatal(e)
	}
	var marker, u, claims int64
	if e = conn.QueryRow(c, `SELECT complete_before_height,unique_planets,held_planets FROM structs.influence_rollup_coverage r JOIN structs.influence_checkpoint k USING(build_id) WHERE height=999`).Scan(&marker, &u, &claims); e != nil || marker != 1000 || u != 1 || claims != 1 {
		t.Fatalf("rollup %d %d %d: %v", marker, u, claims, e)
	}
	tx, e = conn.Begin(c)
	if e != nil {
		t.Fatal(e)
	}
	tr = start(t, c, tx, 1000)
	move(t, c, tx, tr, "1-a", "0-b")
	finish(t, c, tx, tr)
	if _, e = tx.Exec(c, `DO $$ BEGIN FOR h IN 1001..1999 LOOP PERFORM structs.influence_finish_block('influence-test',h,'hash-'||h); END LOOP; END $$`); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(c); e != nil {
		t.Fatal(e)
	}
	if e = Refresh(c, conn, io.Discard); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(c, `SELECT count(*) FROM structs.influence_checkpoint k JOIN structs.influence_guild_sector s USING(build_id,guild,sector) WHERE height=1999 AND (k.unique_planets<>s.unique_planets OR k.held_planets<>s.held_planets)`).Scan(&u); e != nil || u != 0 {
		t.Fatalf("chart endpoint differs %d %v", u, e)
	}
	if _, e = conn.Exec(c, `SELECT structs.influence_rebuild($1)`, id); e != nil {
		t.Fatal(e)
	}
	if e = Refresh(c, conn, io.Discard); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(c, `SELECT count(*) FROM structs.influence_published_build p JOIN structs.influence_rollup_coverage r USING(build_id) WHERE p.contest_id=$1 AND complete_before_height=2000 AND refreshed_revision=build_id`, id).Scan(&u); e != nil || u != 1 {
		t.Fatalf("late rebuilt rollup %d %v", u, e)
	}
}
func start(t *testing.T, c context.Context, tx pgx.Tx, h int64) *Tracker {
	t.Helper()
	r, e := Begin(c, tx, "influence-test", h, fmt.Sprint("hash-", h), time.Unix(h, 0).UTC(), false)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func event(kind, key string, v any, index, msg int) rpc.PositionedEvent {
	b, _ := json.Marshal(v)
	return rpc.PositionedEvent{Phase: rpc.Transaction, TxIndex: 0, MsgIndex: msg, EventIndex: index, Event: rpc.Event{Type: "structs.structs." + kind, Attributes: []rpc.Attribute{{Key: key, Value: string(b)}}}}
}
func raid(t *testing.T, c context.Context, tx pgx.Tx, tr *Tracker, a, p string, msg int) {
	t.Helper()
	for _, e := range []rpc.PositionedEvent{
		event("EventOreTheft", "eventOreTheftDetail", map[string]string{"thiefPlayerId": "1-" + a, "victimPlayerId": "1-d", "amount": "0"}, msg*20+4, msg),
		event("EventRaid", "eventRaidDetail", map[string]string{"fleetId": "9-" + a, "planetId": p, "status": "raidSuccessful", "seized_ore": "0"}, msg*20+7, msg)} {
		if err := tr.Observe(c, tx, e); err != nil {
			t.Fatal(err)
		}
	}
}
func finish(t *testing.T, c context.Context, tx pgx.Tx, tr *Tracker) {
	t.Helper()
	if e := tr.Finish(c, tx); e != nil {
		t.Fatal(e)
	}
}
func create(t *testing.T, c context.Context, tx pgx.Tx, s, e int64) int64 {
	t.Helper()
	var id int64
	if err := tx.QueryRow(c, `SELECT structs.influence_create_contest('influence-test',$1,$2)`, s, e).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func points(t *testing.T, c context.Context, tx pgx.Tx, id int64, g string) int64 {
	t.Helper()
	var n int64
	if e := tx.QueryRow(c, `SELECT coalesce(sum(points),0)::bigint FROM structs.influence_standings WHERE contest_id=$1 AND guild=$2`, id, g).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func reconcile(t *testing.T, c context.Context, tx pgx.Tx, id int64) {
	t.Helper()
	var n int64
	e := tx.QueryRow(c, `SELECT sum(mismatches)::bigint FROM structs.influence_published_build p CROSS JOIN LATERAL structs.influence_reconcile(p.build_id) r WHERE p.contest_id=$1`, id).Scan(&n)
	if e != nil || n != 0 {
		t.Fatalf("reconcile %d %v", n, e)
	}
}
func move(t *testing.T, c context.Context, tx pgx.Tx, tr *Tracker, p, g string) {
	t.Helper()
	if e := tr.Observe(c, tx, event("EventPlayer", "player", map[string]string{"id": p, "guildId": g}, 1, 0)); e != nil {
		t.Fatal(e)
	}
	if _, e := tx.Exec(c, `UPDATE structs.player SET guild_id=$2 WHERE id=$1`, p, g); e != nil {
		t.Fatal(e)
	}
}
func TestIntegrationCoverageTransferRebuildFinal(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 3)
	raid(t, c, tx, tr, "a", "2-a", 0)
	raid(t, c, tx, tr, "b", "2-a", 1)
	raid(t, c, tx, tr, "a", "2-b", 2)
	finish(t, c, tx, tr)
	if n := points(t, c, tx, id, "0-a"); n != 600 {
		t.Fatalf("two planets same defender: %d", n)
	}
	tr = start(t, c, tx, 2)
	move(t, c, tx, tr, "1-a", "0-b")
	finish(t, c, tx, tr)
	if n := points(t, c, tx, id, "0-a"); n != 300 {
		t.Fatal(n)
	}
	if n := points(t, c, tx, id, "0-b"); n != 400 {
		t.Fatal(n)
	}
	reconcile(t, c, tx, id)
	var b int64
	if e := tx.QueryRow(c, `SELECT structs.influence_rebuild($1)`, id).Scan(&b); e != nil {
		t.Fatal(e)
	}
	reconcile(t, c, tx, id)
	if points(t, c, tx, id, "0-b") != 400 {
		t.Fatal("rebuild differs")
	}
	tr = start(t, c, tx, 3)
	finish(t, c, tx, tr)
	tr = start(t, c, tx, 3)
	finish(t, c, tx, tr)
	tr = start(t, c, tx, 4)
	move(t, c, tx, tr, "1-a", "0-a")
	finish(t, c, tx, tr)
	if points(t, c, tx, id, "0-b") != 400 {
		t.Fatal("changed final")
	}
}
func TestIntegrationRangeAndIneligibleClaim(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 4)
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	tr = start(t, c, tx, 2)
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	other := create(t, c, tx, 2, 2)
	if points(t, c, tx, other, "0-a") != 300 {
		t.Fatal("range uniqueness")
	}
	tr = start(t, c, tx, 3)
	move(t, c, tx, tr, "1-b", "0-d")
	raid(t, c, tx, tr, "b", "2-a", 1)
	finish(t, c, tx, tr)
	if points(t, c, tx, id, "0-a") != 100 {
		t.Fatal("old claim not cleared")
	}
	if points(t, c, tx, id, "0-d") != 0 {
		t.Fatal("same guild scored")
	}
	reconcile(t, c, tx, id)
}
func TestIntegrationMissingTheft(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	if e := tr.Observe(c, tx, event("EventRaid", "eventRaidDetail", map[string]string{"fleetId": "9-a", "planetId": "2-a", "status": "raidSuccessful", "seized_ore": "0"}, 7, 0)); e == nil {
		t.Fatal("unmatched accepted")
	}
}
func TestIntegrationUnassignedSector(t *testing.T) {
	c, tx := fixture(t)
	if _, e := tx.Exec(c, `UPDATE structs.planet SET sector=0 WHERE id='2-a'`); e != nil {
		t.Fatal(e)
	}
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 1)
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	var state string
	var ready bool
	if e := tx.QueryRow(c, `SELECT state,sectors_complete FROM structs.influence_standings WHERE contest_id=$1`, id).Scan(&state, &ready); e != nil {
		t.Fatal(e)
	}
	if state == "final" || ready {
		t.Fatal("unassigned finalized")
	}
}
func TestIntegrationRewind(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 2)
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	tr = start(t, c, tx, 2)
	raid(t, c, tx, tr, "b", "2-a", 0)
	finish(t, c, tx, tr)
	if _, e := tx.Exec(c, `INSERT INTO structs.current_block(chain,height) VALUES('influence-test',1); SELECT structs.influence_rewind('influence-test',1,'hash-1')`); e != nil {
		t.Fatal(e)
	}
	var h string
	if e := tx.QueryRow(c, `SELECT holder FROM structs.influence_planet_claim c JOIN structs.influence_published_build p USING(build_id) WHERE p.contest_id=$1`, id).Scan(&h); e != nil {
		t.Fatal(e)
	}
	if h != "1-a" {
		t.Fatal(h)
	}
	reconcile(t, c, tx, id)
	tr = start(t, c, tx, 2)
	raid(t, c, tx, tr, "b", "2-a", 0)
	finish(t, c, tx, tr)
	reconcile(t, c, tx, id)
}
