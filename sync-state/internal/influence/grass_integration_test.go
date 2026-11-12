package influence

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIntegrationGrassStateHistoryAndRebuild(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 4)
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	read := func(scope string) map[string]any {
		t.Helper()
		var raw []byte
		if err := tx.QueryRow(c, `SELECT payload FROM structs.influence_grass_frame WHERE contest_id=$1 AND subject LIKE '%.'||$2 AND payload->>'category'<>'influence_history' ORDER BY frame_id DESC LIMIT 1`, id, scope).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	data := func(scope string) map[string]any { return read(scope)["data"].(map[string]any) }
	if p := data("player.1-a"); p["points"] != "300" || p["additive_to_guild"] != false {
		t.Fatal(p)
	}
	if p := data("planet.2-a"); p["holder"] != "1-a" || p["sector"] != float64(127) {
		t.Fatal(p)
	}
	// Same-guild holder replacement: guild points stay 300, both personals change.
	tr = start(t, c, tx, 2)
	raid(t, c, tx, tr, "b", "2-a", 0)
	finish(t, c, tx, tr)
	if data("player.1-a")["points"] != "100" || data("player.1-b")["points"] != "300" {
		t.Fatal("lost personal-only change")
	}
	guild := data("guild.0-a")
	if len(guild["players"].([]any)) != 2 {
		t.Fatal("guild roster patch incomplete", guild)
	}
	if h := guild["history"].(map[string]any)["samples"].([]any); len(h) != 0 {
		t.Fatal("invented guild delta", h)
	}
	// Two moves inside one block must retain the opening guild for removals.
	tr = start(t, c, tx, 3)
	if _, e := tx.Exec(c, `SELECT structs.influence_record_member('{"chain":"influence-test","height":3,"phase":2,"tx_index":0,"event_index":1,"time":"1970-01-01T00:00:03Z"}','1-b','0-intermediate');
 SELECT structs.influence_record_member('{"chain":"influence-test","height":3,"phase":2,"tx_index":0,"event_index":2,"time":"1970-01-01T00:00:03Z"}','1-b','0-new')`); e != nil {
		t.Fatal(e)
	}
	finish(t, c, tx, tr)
	p := data("player.1-b")
	if p["previous_guild_id"] != "0-a" || p["guild_id"] != "0-new" || p["points"] != "300" {
		t.Fatal(p)
	}
	if len(data("guild.0-a")["players"].([]any)) != 1 || len(data("guild.0-new")["players"].([]any)) != 1 {
		t.Fatal("transfer scope missing")
	}
	if data("planet.2-a")["holder_guild_id"] != "0-new" {
		t.Fatal("planet membership credit stale")
	}
	// The empty final block still announces finality, and a repeated Finish is inert.
	tr = start(t, c, tx, 4)
	finish(t, c, tx, tr)
	if read("scores")["context"].(map[string]any)["status"] != "final" {
		t.Fatal("missing empty-block final")
	}
	var before, after int
	_ = tx.QueryRow(c, `SELECT count(*) FROM structs.influence_grass_frame`).Scan(&before)
	tr = start(t, c, tx, 4)
	finish(t, c, tx, tr)
	_ = tx.QueryRow(c, `SELECT count(*) FROM structs.influence_grass_frame`).Scan(&after)
	if before != after {
		t.Fatal("duplicate finish enqueued frames")
	}
	// Rebuild sends replacement state + complete exact personal history.
	if _, e := tx.Exec(c, `SELECT structs.influence_rebuild($1)`, id); e != nil {
		t.Fatal(e)
	}
	f := read("player.1-b")
	if f["mode"] != "replace" {
		t.Fatal(f)
	}
	p = f["data"].(map[string]any)
	h := p["history"].(map[string]any)
	var history []byte
	if err := tx.QueryRow(c, `SELECT payload->'data'->'history' FROM structs.influence_grass_frame WHERE contest_id=$1 AND subject LIKE '%.player.1-b' AND payload->>'category'='influence_history' ORDER BY frame_id DESC LIMIT 1`, id).Scan(&history); err != nil {
		t.Fatal(err)
	}
	var completed map[string]any
	if err := json.Unmarshal(history, &completed); err != nil {
		t.Fatal(err)
	}
	if completed["complete"] != true || len(completed["samples"].([]any)) != 1 {
		t.Fatal("incomplete replacement history", completed)
	}
	if h["opening_is_zero"] != true || p["points"] != "300" {
		t.Fatal(p)
	}
	// Rewind removes player B's historical win and produces an explicit zero snapshot.
	if _, e := tx.Exec(c, `INSERT INTO structs.current_block VALUES('influence-test',1); SELECT structs.influence_rewind('influence-test',1,'hash-1')`); e != nil {
		t.Fatal(e)
	}
	p = data("player.1-b")
	if p["points"] != "0" || len(p["history"].(map[string]any)["samples"].([]any)) != 0 {
		t.Fatal("stale history survived rewind", p)
	}
	if data("planet.2-a")["holder"] != "1-a" {
		t.Fatal("rewind claim missing")
	}
}

func TestIntegrationGrassRollbackAndBulk(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	create(t, c, tx, 1, 3)
	var baseline int
	_ = tx.QueryRow(c, `SELECT count(*) FROM structs.influence_grass_frame`).Scan(&baseline)
	nested, e := tx.Begin(c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = nested.Exec(c, `SET LOCAL structs.bulk_deferred='on'`); e != nil {
		t.Fatal(e)
	}
	raid(t, c, nested, tr, "a", "2-a", 0)
	finish(t, c, nested, tr)
	var n int
	if e = nested.QueryRow(c, `SELECT count(*) FROM structs.influence_grass_frame WHERE height=1 AND payload->>'mode'='patch'`).Scan(&n); e != nil || n == 0 {
		t.Fatalf("bulk suppressed frames: %d %v", n, e)
	}
	if e = nested.Rollback(c); e != nil {
		t.Fatal(e)
	}
	if e = tx.QueryRow(c, `SELECT count(*) FROM structs.influence_grass_frame`).Scan(&n); e != nil || n != baseline {
		t.Fatalf("rollback leaked frames %d %d %v", n, baseline, e)
	}
	if e = tx.QueryRow(c, `SELECT count(*) FROM structs.influence_player_score_change`).Scan(&n); e != nil || n != 0 {
		t.Fatal("rollback leaked personal history", n, e)
	}
}

func TestIntegrationGrassBoundedPlayerFanout(t *testing.T) {
	c, tx := fixture(t)
	tr := start(t, c, tx, 1)
	id := create(t, c, tx, 1, 3)
	if _, e := tx.Exec(c, `INSERT INTO structs.influence_member(build_id,player,guild)
 SELECT build_id,'scale-'||i,'0-scale-'||(i%6) FROM structs.influence_published_build CROSS JOIN generate_series(1,10000) i WHERE contest_id=$1`, id); e != nil {
		t.Fatal(e)
	}
	began := time.Now()
	raid(t, c, tx, tr, "a", "2-a", 0)
	finish(t, c, tx, tr)
	var frames, players int
	if e := tx.QueryRow(c, `SELECT count(*),count(*) FILTER(WHERE payload->>'category'='influence_player') FROM structs.influence_grass_frame WHERE contest_id=$1 AND height=1 AND payload->>'mode'='patch'`, id).Scan(&frames, &players); e != nil {
		t.Fatal(e)
	}
	if frames > 20 || players != 1 {
		t.Fatalf("broadcast unaffected players: frames=%d players=%d", frames, players)
	}
	t.Logf("10,003 members: one raid produced %d scoped frames in %s", frames, time.Since(began))
}

func TestIntegrationGrassHistorySegments(t *testing.T) {
	c, tx := fixture(t)
	start(t, c, tx, 1)
	id := create(t, c, tx, 1, 2000)
	var build int64
	if e := tx.QueryRow(c, `SELECT build_id FROM structs.influence_published_build WHERE contest_id=$1`, id).Scan(&build); e != nil {
		t.Fatal(e)
	}
	// Isolate delivery segmentation from gameplay: a long synthetic personal
	// ledger exercises multiple bounded segments without 1,200 block transactions.
	if _, e := tx.Exec(c, `UPDATE structs.influence_projection_build SET projected_height=1200 WHERE build_id=$1;
 `, build); e != nil {
		t.Fatal(e)
	}
	if _, e := tx.Exec(c, `INSERT INTO structs.influence_player_score_change SELECT $1,i,'1-a',127,1,0 FROM generate_series(1,1200) i`, build); e != nil {
		t.Fatal(e)
	}
	if _, e := tx.Exec(c, `SELECT structs.influence_grass_emit_history($1,'player.1-a','player','1-a')`, build); e != nil {
		t.Fatal(e)
	}
	var total, maxRows, completed int
	if e := tx.QueryRow(c, `SELECT sum(jsonb_array_length(payload->'data'->'history'->'samples')),
 max(jsonb_array_length(payload->'data'->'history'->'samples')),
 count(*) FILTER(WHERE payload->'data'->'history'->>'complete'='true')
 FROM structs.influence_grass_frame WHERE build_id=$1 AND height=1200 AND payload->>'category'='influence_history'`, build).Scan(&total, &maxRows, &completed); e != nil {
		t.Fatal(e)
	}
	if total != 1200 || maxRows != 512 || completed != 1 {
		t.Fatalf("history segmentation: %d %d %d", total, maxRows, completed)
	}
	var unique int
	if e := tx.QueryRow(c, `SELECT count(DISTINCT row->>'height') FROM structs.influence_grass_frame,
 LATERAL jsonb_array_elements(payload->'data'->'history'->'samples') row
 WHERE build_id=$1 AND height=1200 AND payload->>'category'='influence_history'`, build).Scan(&unique); e != nil || unique != 1200 {
		t.Fatalf("missing history rows %d %v", unique, e)
	}
}
