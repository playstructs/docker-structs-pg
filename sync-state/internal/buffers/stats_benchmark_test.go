package buffers

import (
	"context"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

// Same 2,000 points, timestamps and table; only COPY frequency differs.
func BenchmarkStatisticsWindow(b *testing.B) {
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
	stamp := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
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
				buf := New()
				for h := int64(1); h <= 500 && err == nil; h++ {
					for i := 0; i < 4; i++ {
						buf.StatStructHealth = append(buf.StatStructHealth, StatRow{Time: stamp.Add(time.Duration(h) * time.Second), ObjectIndex: 999910009 + i, Value: "42", BlockHeight: h})
					}
					if batch {
						_, err = buf.FlushBlock(ctx, tx)
					} else {
						_, err = buf.Flush(ctx, tx)
					}
				}
				if err == nil {
					err = buf.FlushStats(ctx, tx)
				}
				_ = tx.Rollback(ctx)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
