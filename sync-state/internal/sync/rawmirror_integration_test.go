package sync

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/rpc"
)

// A large block may push a batch over its limit. Replaying the window must
// replace each complete height, including the final partial batch.
func TestRawEventBatchReplay(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	event := rpc.Event{Type: "transfer", Attributes: []rpc.Attribute{
		{Key: "sender", Value: "expected"}, {Key: "sender", Value: "duplicate"},
	}}
	large := make([]rpc.Event, 4200)
	for i := range large {
		large[i] = event
	}
	const chain = "raw-batch-test"
	blocks := []BlockBundle{
		{ChainID: chain, Height: 1, FinalizeBlockEvents: []rpc.Event{event}},
		{ChainID: chain, Height: 2, FinalizeBlockEvents: large},
		{ChainID: chain, Height: 3, RawTxs: []string{"AQ=="}, TxResults: []rpc.TxResult{{Events: []rpc.Event{event}}}},
	}
	s := &Syncer{}
	for pass := 0; pass < 2; pass++ {
		var batch rawEventBatch
		for i := range blocks {
			if err := s.writeRawMirror(ctx, tx, &blocks[i], &batch); err != nil {
				t.Fatal(err)
			}
		}
		if len(batch.events) != 1 {
			t.Fatalf("expected only the final block to be buffered, got %d events", len(batch.events))
		}
		if err := batch.flush(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	var events, attrs, txAttrs int
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM sync_state.raw_events WHERE chain_id=$1),
		count(*), count(*) FILTER (WHERE height=3 AND tx_index=0 AND event_index=0)
		FROM sync_state.raw_attributes WHERE chain_id=$1 AND value='expected'`, chain).Scan(&events, &attrs, &txAttrs)
	if err != nil || events != 4202 || attrs != 4202 || txAttrs != 1 {
		t.Fatalf("replayed events=%d attributes=%d tx attributes=%d err=%v", events, attrs, txAttrs, err)
	}
}
