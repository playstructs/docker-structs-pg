package sync

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// GuardReplay against sync_state bookkeeping, inside a rolled-back tx.
//
//	INTEGRATION_DATABASE_URL=postgres://structs@localhost:5432/structs?sslmode=disable \
//	    go test ./internal/sync -run TestGuardReplay -v
func TestGuardReplay(t *testing.T) {
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	const chain = "replay-guard-test"

	cases := []struct {
		name        string
		cursor      int64
		blockLog    int64
		otherCursor int64
		start       int64
		allow       bool
		wantStart   int64
		wantNotice  bool
		wantErr     string
	}{
		{name: "fresh database", start: 1, wantStart: 1},
		{name: "normal resume", cursor: 500, blockLog: 500, start: 501, wantStart: 501},
		{name: "skip ahead", cursor: 500, blockLog: 500, start: 900, wantStart: 900},
		{name: "start override below cursor resumes", cursor: 500, blockLog: 500, start: 1, wantStart: 501, wantNotice: true},
		{name: "lost cursor refused", blockLog: 500, start: 1, wantErr: "block_log reaches 500"},
		{name: "cursor behind block_log refused", cursor: 200, blockLog: 500, start: 201, wantErr: "block_log reaches 500"},
		{name: "other chain refused", otherCursor: 900, start: 1, wantErr: "second chain"},
		{name: "allow replay", cursor: 500, blockLog: 500, start: 1, allow: true, wantStart: 1, wantNotice: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := pgx.Connect(ctx, url)
			if err != nil {
				t.Fatalf("pg connect: %v", err)
			}
			defer conn.Close(context.Background())
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(context.Background())

			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := tx.Exec(ctx, sql, args...); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			exec(`DELETE FROM sync_state.sync_cursor`)
			if tc.cursor > 0 {
				exec(`INSERT INTO sync_state.sync_cursor (chain_id, last_height) VALUES ($1, $2)`, chain, tc.cursor)
			}
			if tc.otherCursor > 0 {
				exec(`INSERT INTO sync_state.sync_cursor (chain_id, last_height) VALUES ($1, $2)`, chain+"-other", tc.otherCursor)
			}
			if tc.blockLog > 0 {
				exec(`INSERT INTO sync_state.block_log (chain_id, height, block_hash, block_time, num_txs, num_events)
				      VALUES ($1, $2, 'hash', NOW(), 0, 0)`, chain, tc.blockLog)
			}

			start, notice, err := GuardReplay(ctx, tx, chain, tc.start, tc.allow)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GuardReplay: %v", err)
			}
			if start != tc.wantStart {
				t.Errorf("start = %d; want %d", start, tc.wantStart)
			}
			if (notice != "") != tc.wantNotice {
				t.Errorf("notice = %q; want notice=%v", notice, tc.wantNotice)
			}
		})
	}
}
