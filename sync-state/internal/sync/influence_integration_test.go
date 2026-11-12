package sync

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sync-state/internal/buffers"
	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/rpc"
)

// Uses the full Sqitch game schema, the real router, buffer flush and block
// transaction in both streaming and deferred-projection (bulk) mode.
func TestInfluenceActualBlock(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	c := context.Background()
	pool, e := pgxpool.New(c, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	if e = db.Bootstrap(c, pool); e != nil {
		t.Fatal(e)
	}
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "bulk"}[deferred], func(t *testing.T) {
			tx, e := pool.Begin(c)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(c)
			_, e = tx.Exec(c, `INSERT INTO structs.player(id,guild_id,primary_address) VALUES('1-99101','0-99101','structs1q3nx9j4qcqaf4p2qjc9zxqxrccg4renc2fyaw5'),('1-99102','0-99102','structs1rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr');
 INSERT INTO structs.fleet(id,owner) VALUES('9-99101','1-99101');
 INSERT INTO structs.planet(id,owner,sector) VALUES('2-99101','1-99102',127)`)
			if e != nil {
				t.Fatal(e)
			}
			at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			const chain = "influence-block-test"
			if _, e = tx.Exec(c, `SELECT structs.influence_begin_block($1,1,'hash-1',$2,false)`, chain, at); e != nil {
				t.Fatal(e)
			}
			var cid int64
			if e = tx.QueryRow(c, `SELECT structs.influence_create_contest($1,1,2)`, chain).Scan(&cid); e != nil {
				t.Fatal(e)
			}
			ev := func(kind, key string, v any) rpc.Event {
				b, _ := json.Marshal(v)
				return rpc.Event{Type: "structs.structs." + kind, Attributes: []rpc.Attribute{{Key: key, Value: string(b)}, {Key: "msg_index", Value: "0"}}}
			}
			theft := ev("EventOreTheft", "eventOreTheftDetail", map[string]string{"thiefPlayerId": "1-99101", "victimPlayerId": "1-99102", "thiefPrimaryAddress": "structs1q3nx9j4qcqaf4p2qjc9zxqxrccg4renc2fyaw5", "victimPrimaryAddress": "structs1rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr", "amount": "7"})
			raid := ev("EventRaid", "eventRaidDetail", map[string]string{"fleetId": "9-99101", "planetId": "2-99101", "status": "raidSuccessful", "seized_ore": "7"})
			bad := ev("EventPlayer", "player", map[string]string{"id": "1-99101", "guildId": "0-99102"})
			s := NewSyncer(Config{}, nil, &db.Pool{Pool: pool}, events.NewRouter(false), chain)
			bundle := &BlockBundle{ChainID: chain, Height: 1, BlockHashHex: "hash-1", BlockTime: at, TxResults: []rpc.TxResult{{Code: 1, Events: []rpc.Event{bad}}, {Code: 0, Events: []rpc.Event{theft, raid}}}}
			if _, e = s.applyBlockInTx(c, tx, bundle, 2, buffers.New(), applyOpts{DeferProjections: deferred}); e != nil {
				t.Fatal(e)
			}
			var points int64
			if e = tx.QueryRow(c, `SELECT points::bigint FROM structs.influence_standings WHERE contest_id=$1`, cid).Scan(&points); e != nil || points != 300 {
				t.Fatalf("score=%d %v", points, e)
			}
			var framePoints string
			if e = tx.QueryRow(c, `SELECT row->>'points' FROM structs.influence_grass_frame f,
              LATERAL jsonb_array_elements(f.payload->'data'->'standings') row
              WHERE contest_id=$1 AND height=1 AND payload->>'category'='influence_scores' AND payload->>'mode'='patch'
              AND row->>'guild_id'='0-99101' AND row->>'sector'='127'`, cid).Scan(&framePoints); e != nil || framePoints != "300" {
				t.Fatalf("grass score %s %v", framePoints, e)
			}
			var guild string
			if e = tx.QueryRow(c, `SELECT guild_id FROM structs.player WHERE id='1-99101'`).Scan(&guild); e != nil || guild != "0-99101" {
				t.Fatalf("failed transaction changed player: %s %v", guild, e)
			}
			// Same raid in the next block is a new fact, even when planet_raid's
			// unchanged-status guard suppresses its ordinary activity update.
			bundle.Height = 2
			bundle.BlockHashHex = "hash-2"
			bundle.BlockTime = at.Add(time.Second)
			bundle.TxResults = []rpc.TxResult{{Events: []rpc.Event{theft, raid}}}
			if _, e = s.applyBlockInTx(c, tx, bundle, 2, buffers.New(), applyOpts{DeferProjections: deferred}); e != nil {
				t.Fatal(e)
			}
			var n int
			if e = tx.QueryRow(c, `SELECT count(*) FROM structs.influence_raid WHERE chain_id=$1`, chain).Scan(&n); e != nil || n != 2 {
				t.Fatalf("lost repeated success: %d %v", n, e)
			}
			var state string
			if e = tx.QueryRow(c, `SELECT state,points::bigint FROM structs.influence_standings WHERE contest_id=$1`, cid).Scan(&state, &points); e != nil || state != "final" || points != 300 {
				t.Fatalf("final %s %d %v", state, points, e)
			}
		})
	}
}
