package db

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
)

// Synthetic 500-block window, four small transactions per block. This isolates
// transaction-result COPY overhead; it is not an end-to-end ingestion forecast.
func BenchmarkRawTransactionWindow(b *testing.B) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		b.Skip("requires disposable database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close(ctx)
	rows := make([]RawMirrorTxResult, 0, 2000)
	heights := make([]int64, 500)
	for h := range heights {
		heights[h] = int64(h + 1)
		for i := 0; i < 4; i++ {
			rows = append(rows, RawMirrorTxResult{ChainID: "benchmark-raw-window", Height: int64(h + 1), TxIndex: i, RawJSON: json.RawMessage(`{"code":0,"gas_used":"1234"}`)})
		}
	}
	for _, batch := range []bool{false, true} {
		name := "per_block"
		if batch {
			name = "window"
		}
		b.Run(name, func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				tx, err := conn.Begin(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if batch {
					err = WriteRawTxResultsAtHeights(ctx, tx, rows[0].ChainID, heights, rows)
				} else {
					for i := 0; i < len(rows) && err == nil; i += 4 {
						err = WriteRawTxResults(ctx, tx, rows[i:i+4])
					}
				}
				_ = tx.Rollback(ctx)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
