package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/rpc"
)

func TestRequiredFailure_RollsBackBlockAndWindow(t *testing.T) {
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckEconomySchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"block", "window", "bank", "bank-window", "sql", "sql-window"} {
		t.Run(mode, func(t *testing.T) {
			chain := "required-failure-" + mode
			t.Cleanup(func() {
				_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id=$1`, chain)
				_, _ = pool.Exec(ctx, `DELETE FROM structs.current_block WHERE chain=$1`, chain)
			})
			s := NewSyncer(Config{BulkDeferProjections: true, MirrorRaw: true, SlowBlockThreshold: time.Nanosecond}, nil, &db.Pool{Pool: pool}, events.NewRouter(false), chain)
			var timingLog bytes.Buffer
			s.logger = log.New(&timingLog, "", 0)
			good := &BlockBundle{Height: 990991, ChainID: chain, BlockTime: time.Now().UTC(), BlockHashHex: "fixture"}
			good.FinalizeBlockEvents = []rpc.Event{{Type: "structs.structs.EventVaultAddress", Attributes: []rpc.Attribute{
				{Key: "eventVaultAddressDetail", Value: `{"ownerId":"1-990991","vaultAddress":"atomic-vault-test","accountNumber":"0","accountCreated":false}`},
			}}}
			good.FinalizeBlockEvents = append(good.FinalizeBlockEvents, rpc.Event{
				Type: "structs.structs.EventRaid", Attributes: []rpc.Attribute{{
					Key: "eventRaidDetail", Value: `{"planetId":"2-999920001","fleetId":"9-999920001","status":"started","seizedOre":"0"}`,
				}},
			})
			bad := &BlockBundle{Height: good.Height + 1, ChainID: chain, BlockTime: good.BlockTime, BlockHashHex: "fixture-bad"}
			bad.FinalizeBlockEvents = []rpc.Event{
				{Type: "structs.structs.EventReactorCommissionRevisionReset"},
				{Type: "structs.structs.EventReactorStakeCap", Attributes: []rpc.Attribute{{Key: "active", Value: "true"}}},
			}
			wantErrorKey := "structs.structs.EventReactorStakeCap"
			sqlFailure := strings.HasPrefix(mode, "sql")
			if sqlFailure {
				// Unknown attribute labels violate the database NOT NULL
				// constraint. This aborts the actual PostgreSQL transaction,
				// unlike a payload validation error returned before SQL.
				wantErrorKey = "structs.structs.EventStructAttribute.structAttributeRecord"
				bad.FinalizeBlockEvents = []rpc.Event{{Type: "structs.structs.EventStructAttribute", Attributes: []rpc.Attribute{{
					Key: "structAttributeRecord", Value: `{"attributeId":"99-5-999920001","value":"1"}`,
				}}}}
			}
			bankFailure := mode == "bank" || mode == "bank-window"
			if bankFailure {
				wantErrorKey = "bank.process_block"
				// Buffer both transfer legs before the invalid unbond aborts the block.
				bad.FinalizeBlockEvents = []rpc.Event{{Type: "transfer", Attributes: []rpc.Attribute{
					{Key: "sender", Value: "bank-rollback-sender"}, {Key: "recipient", Value: "bank-rollback-recipient"},
					{Key: "amount", Value: "10ualpha"},
				}}, {Type: "unbond", Attributes: []rpc.Attribute{
					{Key: "amount", Value: "1ualpha"}, {Key: "validator", Value: "validator-test"},
					{Key: "delegator", Value: "delegator-test"}, {Key: "completion_time", Value: "invalid"},
				}}}
			}
			if strings.Contains(mode, "window") {
				if _, e := pool.Exec(ctx, `INSERT INTO structs.current_block(chain,height) VALUES($1,$2)`, chain, good.Height-1); e != nil {
					t.Fatal(e)
				}
				err = s.applyBulkWindow(ctx, []*BlockBundle{good, bad}, bad.Height)
			} else {
				if _, e := pool.Exec(ctx, `INSERT INTO structs.current_block(chain,height) VALUES($1,$2)`, chain, bad.Height-1); e != nil {
					t.Fatal(e)
				}
				bad.FinalizeBlockEvents = append(good.FinalizeBlockEvents, bad.FinalizeBlockEvents...)
				err = s.applyBlock(ctx, bad, bad.Height)
			}
			if err == nil || !strings.Contains(err.Error(), map[bool]string{true: "bank", false: "required event"}[bankFailure]) {
				t.Fatalf("expected required failure, got %v", err)
			}
			var report map[string]any
			if e := json.Unmarshal(bytes.TrimSpace(timingLog.Bytes()), &report); e != nil {
				t.Fatalf("timing log: %s: %v", timingLog.String(), e)
			}
			if report["outcome"] != "failed" {
				t.Fatal(report)
			}
			phases := report["phases_ms"].(map[string]any)
			for _, phase := range []string{"dispatch", "rollback", "handler_error_log"} {
				if _, ok := phases[phase]; !ok {
					t.Fatalf("missing %s: %v", phase, report)
				}
			}
			if sqlFailure && !strings.Contains(err.Error(), "23502") {
				t.Fatalf("expected database NOT NULL violation, got %v", err)
			}
			for _, query := range []string{
				`SELECT count(*) FROM structs.influence_source_cursor WHERE chain_id=$1`,
				`SELECT count(*) FROM structs.influence_membership_history WHERE chain_id=$1`,
				`SELECT count(*) FROM structs.vault WHERE address='atomic-vault-test'`,
				`SELECT count(*) FROM structs.planet_activity_sequence WHERE planet_id='2-999920001'`,
				`SELECT count(*) FROM structs.planet_activity WHERE planet_id='2-999920001'`,
				`SELECT count(*) FROM sync_state.sync_cursor WHERE chain_id=$1`,
				`SELECT count(*) FROM sync_state.block_log WHERE chain_id=$1`,
				`SELECT count(*) FROM sync_state.raw_blocks WHERE chain_id=$1`,
				`SELECT count(*) FROM sync_state.raw_events WHERE chain_id=$1`,
				`SELECT count(*) FROM sync_state.raw_attributes WHERE chain_id=$1`,
				`SELECT count(*) FROM structs.ledger WHERE chain_id=$1 AND block_height=990992`,
			} {
				var count int
				var args []any
				if strings.Contains(query, "$1") {
					args = []any{chain}
				}
				if err := pool.QueryRow(ctx, query, args...).Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed transaction left state: %s count=%d err=%v", query, count, err)
				}
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM sync_state.handler_error_log WHERE chain_id=$1 AND composite_key=$2 AND severity='error'`, chain, wantErrorKey).Scan(&count); err != nil || count != 1 {
				t.Fatalf("diagnostic did not survive rollback: %d %v", count, err)
			}
		})
	}
}
