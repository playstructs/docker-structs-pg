package sync

import (
	"context"
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
	for _, mode := range []string{"block", "window", "bank"} {
		t.Run(mode, func(t *testing.T) {
			chain := "required-failure-" + mode
			t.Cleanup(func() {
				_, _ = pool.Exec(ctx, `DELETE FROM sync_state.handler_error_log WHERE chain_id=$1`, chain)
			})
			s := NewSyncer(Config{BulkDeferProjections: true}, nil, &db.Pool{Pool: pool}, events.NewRouter(false), chain)
			good := &BlockBundle{Height: 990991, ChainID: chain, BlockTime: time.Now().UTC(), BlockHashHex: "fixture"}
			good.FinalizeBlockEvents = []rpc.Event{{Type: "structs.structs.EventVaultAddress", Attributes: []rpc.Attribute{
				{Key: "eventVaultAddressDetail", Value: `{"ownerId":"1-990991","vaultAddress":"atomic-vault-test","accountNumber":"0","accountCreated":false}`},
			}}}
			bad := &BlockBundle{Height: good.Height + 1, ChainID: chain, BlockTime: good.BlockTime, BlockHashHex: "fixture-bad"}
			bad.FinalizeBlockEvents = []rpc.Event{
				{Type: "structs.structs.EventReactorCommissionRevisionReset"},
				{Type: "structs.structs.EventReactorStakeCap", Attributes: []rpc.Attribute{{Key: "active", Value: "true"}}},
			}
			wantErrorKey := "structs.structs.EventReactorStakeCap"
			if mode == "bank" {
				wantErrorKey = "bank.process_block"
				bad.FinalizeBlockEvents = []rpc.Event{{Type: "unbond", Attributes: []rpc.Attribute{
					{Key: "amount", Value: "1ualpha"}, {Key: "validator", Value: "validator-test"},
					{Key: "delegator", Value: "delegator-test"}, {Key: "completion_time", Value: "invalid"},
				}}}
			}
			if mode == "window" {
				err = s.applyBulkWindow(ctx, []*BlockBundle{good, bad}, bad.Height)
			} else {
				bad.FinalizeBlockEvents = append(good.FinalizeBlockEvents, bad.FinalizeBlockEvents...)
				err = s.applyBlock(ctx, bad, bad.Height)
			}
			if err == nil || !strings.Contains(err.Error(), map[bool]string{true: "bank", false: "required event"}[mode == "bank"]) {
				t.Fatalf("expected required failure, got %v", err)
			}
			for _, query := range []string{
				`SELECT count(*) FROM structs.vault WHERE address='atomic-vault-test'`,
				`SELECT count(*) FROM sync_state.sync_cursor WHERE chain_id=$1`,
				`SELECT count(*) FROM sync_state.block_log WHERE chain_id=$1`,
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
