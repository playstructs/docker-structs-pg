package events

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestHandler_GridUnsignedCopy(t *testing.T) {
	conn := connect(t)
	for _, value := range []string{"9223372036854775808", "18446744073709551615", "0"} {
		t.Run(value, func(t *testing.T) {
			inTx(t, conn, func(tx pgx.Tx) {
				ctx := context.Background()
				handle(t, ctx, tx, gridHandler{}, bctx(), mustJSON(t, map[string]string{
					"attributeId": "2-1-990999", "value": value,
				}))
				var grid, stat string
				if err := tx.QueryRow(ctx, `SELECT val::text FROM structs.grid WHERE id='2-1-990999'`).Scan(&grid); err != nil {
					t.Fatal(err)
				}
				if err := tx.QueryRow(ctx, `SELECT value::text FROM structs.stat_capacity WHERE object_type='player' AND object_index=990999`).Scan(&stat); err != nil {
					t.Fatal(err)
				}
				if grid != value || stat != value {
					t.Fatalf("wanted %s: grid=%s copied stat=%s", value, grid, stat)
				}
			})
		})
	}
}

func TestHandler_GridRejectsInvalidUnsigned(t *testing.T) {
	conn := connect(t)
	for _, value := range []string{"-1", "+1", "1.5", "18446744073709551616", "NaN"} {
		t.Run(value, func(t *testing.T) {
			inTx(t, conn, func(tx pgx.Tx) {
				ctx := context.Background()
				bc := bctx()
				err := (gridHandler{}).Handle(ctx, tx, bc, mustJSON(t, map[string]string{
					"attributeId": "2-1-990999", "value": value,
				}))
				if err == nil {
					t.Fatalf("accepted %q", value)
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.grid WHERE id='2-1-990999'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 || len(bc.Buf.StatCapacity) != 0 {
					t.Fatal("invalid value changed state")
				}
			})
		})
	}
}
