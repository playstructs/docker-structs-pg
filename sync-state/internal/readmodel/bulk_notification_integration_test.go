package readmodel

import (
	"context"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

// Real commits prove SET LOCAL does not leak into the next streaming block.
// Use only a disposable database; this fixture leaves no application rows.
func TestDeferredBulkNotificationsAndHistoricalAttribution(t *testing.T) {
	if os.Getenv("INTEGRATION_MAINTENANCE_TESTS") != "1" {
		t.Skip("requires disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, os.Getenv("INTEGRATION_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	listener, err := pgx.Connect(ctx, os.Getenv("INTEGRATION_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(ctx)
	const planet = "2-999991234"
	defer func() {
		for _, stmt := range []string{
			`DELETE FROM structs.planet_activity_player WHERE planet_id=$1`,
			`DELETE FROM structs.planet_activity WHERE planet_id=$1`,
			`DELETE FROM structs.planet WHERE id=$1`,
			`DELETE FROM structs.api_work_dirty WHERE id=$1`,
		} {
			if _, err := conn.Exec(context.Background(), stmt, planet); err != nil {
				t.Error(err)
			}
		}
	}()
	if _, err := listener.Exec(ctx, "LISTEN grass"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{
		// The flag itself needs no superuser privilege. The disposable schema
		// grants this role maintenance access, not direct canonical-table DML.
		`SET LOCAL ROLE structs_indexer`,
		`SET LOCAL structs.bulk_deferred='on'`,
		`RESET ROLE`,
		`INSERT INTO structs.planet(id,owner) VALUES('2-999991234','1-999991234')`,
		`INSERT INTO structs.planet_activity(time,seq,planet_id,category,detail,block_height) VALUES('2026-11-07Z',0,'2-999991234','shield_change','{}',100)`,
		`UPDATE structs.planet SET owner='1-999991235' WHERE id='2-999991234'`,
		`INSERT INTO structs.planet_activity(time,seq,planet_id,category,detail,block_height) VALUES('2026-11-07Z',1,'2-999991234','shield_change','{}',101)`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var owners []string
	var dirty int
	if err := tx.QueryRow(ctx, `SELECT array_agg(player_id ORDER BY seq) FROM structs.planet_activity_player WHERE planet_id=$1`, planet).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if len(owners) != 2 || owners[0] != "1-999991234" || owners[1] != "1-999991235" {
		t.Fatalf("historical ownership=%v", owners)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.api_work_dirty WHERE id=$1`, planet).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("bulk dirty=%d err=%v", dirty, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	quiet, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	notification, err := listener.WaitForNotification(quiet)
	stop()
	if err == nil {
		t.Fatalf("bulk notification escaped: %v", notification)
	}
	// Ordinary streaming retains the work queue and delivery after the bulk commit.
	if _, err := conn.Exec(ctx, `UPDATE structs.planet SET location_list_start='5-999991234' WHERE id=$1`, planet); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM structs.api_work_dirty WHERE id=$1`, planet).Scan(&dirty); err != nil || dirty != 1 {
		t.Fatalf("stream dirty=%d err=%v", dirty, err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO structs.planet_activity(time,seq,planet_id,category,detail,block_height) VALUES('2026-11-07Z',2,$1,'shield_change','{}',102)`, planet); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.WaitForNotification(ctx); err != nil {
		t.Fatalf("stream notification: %v", err)
	}
	// Rollback must also restore the session setting.
	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL structs.bulk_deferred='on'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := conn.QueryRow(ctx, `SELECT current_setting('structs.bulk_deferred',true) IS DISTINCT FROM 'on'`).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("flag leaked after rollback: %v %v", enabled, err)
	}
}
