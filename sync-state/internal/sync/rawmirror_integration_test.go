package sync

import (
	"context"
	"os"
	"strings"
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
	large := make([]rpc.Event, rawEventBatchLimit/2+1)
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
	if err != nil || events != len(large)+2 || attrs != len(large)+2 || txAttrs != 1 {
		t.Fatalf("replayed events=%d attributes=%d tx attributes=%d err=%v", events, attrs, txAttrs, err)
	}

	var txCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sync_state.raw_tx_results WHERE chain_id=$1`, chain).Scan(&txCount); err != nil || txCount != 1 {
		t.Fatalf("replayed transactions=%d err=%v", txCount, err)
	}
	// Replacing a previously populated height with an empty block must clear
	// its transactions too, even when the whole batch has no transaction rows.
	var empty rawEventBatch
	if err := s.writeRawMirror(ctx, tx, &BlockBundle{ChainID: chain, Height: 3}, &empty); err != nil {
		t.Fatal(err)
	}
	if err := empty.flush(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sync_state.raw_tx_results WHERE chain_id=$1`, chain).Scan(&txCount); err != nil || txCount != 0 {
		t.Fatalf("empty replacement transactions=%d err=%v", txCount, err)
	}

	// A few very large transaction payloads trigger the byte cap even when
	// well below the row cap. The complete height is written once.
	var bytes rawEventBatch
	huge := BlockBundle{ChainID: chain, Height: 4, RawTxs: []string{"AQ==", "Ag=="}, TxResults: []rpc.TxResult{{Log: strings.Repeat("x", rawMirrorByteLimit/2)}, {Code: 1, GasUsed: "77"}}}
	if err := s.writeRawMirror(ctx, tx, &huge, &bytes); err != nil {
		t.Fatal(err)
	}
	if len(bytes.heights) != 0 {
		t.Fatal("byte cap did not flush")
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sync_state.raw_tx_results WHERE chain_id=$1 AND height=4`, chain).Scan(&txCount); err != nil || txCount != 2 {
		t.Fatalf("large height transactions=%d err=%v", txCount, err)
	}
}
