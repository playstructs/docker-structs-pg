package readmodel

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This fixture commits synchronization metadata to exercise a real lock wait.
// Run only against the disposable Sqitch database, with maintenance tests opted
// in. The previous metadata is restored; gameplay tables are never seeded here.
func TestScheduledMaintenanceReadsBoundaryAfterWait(t *testing.T) {
	if os.Getenv("INTEGRATION_MAINTENANCE_TESTS") != "1" {
		t.Skip("INTEGRATION_MAINTENANCE_TESTS=1 requires a disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connect := func() *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, os.Getenv("INTEGRATION_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		return conn
	}
	writer, reader := connect(), connect()
	var workCount int
	if err := writer.QueryRow(ctx, `SELECT count(*) FROM view.work_live`).Scan(&workCount); err != nil || workCount != 0 {
		t.Fatalf("committed fixture requires an empty disposable work set: count=%d err=%v", workCount, err)
	}
	tables := []string{"sync_state.sync_cursor", "structs.current_block", "structs.api_refresh_state"}
	prior := map[string]string{}
	for _, table := range tables {
		var data string
		if err := writer.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(t),'[]')::text FROM "+table+" t").Scan(&data); err != nil {
			t.Fatal(err)
		}
		prior[table] = data
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		tx, err := writer.Begin(cleanup)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(cleanup)
		if err := Lock(cleanup, tx); err != nil {
			t.Error(err)
			return
		}
		for _, table := range tables {
			if _, err := tx.Exec(cleanup, "DELETE FROM "+table); err != nil {
				t.Error(err)
				return
			}
			if _, err := tx.Exec(cleanup, "INSERT INTO "+table+" SELECT * FROM jsonb_populate_recordset(NULL::"+table+", $1::jsonb)", prior[table]); err != nil {
				t.Error(err)
				return
			}
		}
		if err := tx.Commit(cleanup); err != nil {
			t.Error(err)
		}
	})
	seed, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(context.Background())
	if err := Lock(ctx, seed); err != nil {
		t.Fatal(err)
	}
	projectionBoundary(t, ctx, seed, 100)
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := Lock(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var command string
	if err := tx.QueryRow(ctx, `SELECT command FROM cron.job WHERE jobname='api-work-refresh-full-hourly'`).Scan(&command); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reader.Exec(ctx, command)
		done <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for waiting := false; !waiting; {
		select {
		case err := <-done:
			t.Fatalf("maintenance did not wait for the writer: %v", err)
		case <-deadline.C:
			t.Fatal("maintenance never reached the advisory lock")
		case <-tick.C:
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event='advisory')`, reader.PgConn().PID()).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
		}
	}
	projectionBoundary(t, ctx, tx, 101)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var height int64
	var sourceTime time.Time
	if err := writer.QueryRow(ctx, `SELECT source_height, source_time FROM structs.api_refresh_state WHERE model='work'`).Scan(&height, &sourceTime); err != nil {
		t.Fatal(err)
	}
	if height != 101 || !sourceTime.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("scheduled repair stamped pre-lock/wall-clock state: height=%d time=%s", height, sourceTime)
	}
}
