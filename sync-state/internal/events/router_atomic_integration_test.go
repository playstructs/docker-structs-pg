package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/readmodel"
)

type atomicProbeHandler struct{ outcome string }

func (atomicProbeHandler) CompositeKey() string { return "test.atomic" }
func (h atomicProbeHandler) Handle(ctx context.Context, tx pgx.Tx, bc BlockContext, _ json.RawMessage) error {
	if h.outcome == "warn" {
		return ErrSkipWithWarn
	}
	if _, err := tx.Exec(ctx, `INSERT INTO atomic_router_probe VALUES (1)`); err != nil {
		return err
	}
	bc.Buf.StatStructHealth = append(bc.Buf.StatStructHealth, buffers.StatRow{Value: "7"})
	bc.Dirty.Player("1-atomic")
	switch h.outcome {
	case "panic":
		panic("atomic probe panic")
	case "error":
		return errors.New("atomic probe error")
	case "sql", "sql-warn", "sql-ignored":
		_, err := tx.Exec(ctx, `SELECT 1/0`)
		if h.outcome == "sql-warn" {
			return ErrSkipWithWarn // A broken handler must not hide SQL failure.
		}
		if h.outcome == "sql-ignored" {
			return nil
		}
		return err
	}
	return nil
}

// Any transaction boundary introduced by DispatchAtomic is a regression.
type atomicProbeTx struct {
	pgx.Tx
	boundaries int
}

func (tx *atomicProbeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	tx.boundaries++
	return tx.Tx.Begin(ctx)
}
func (tx *atomicProbeTx) Commit(ctx context.Context) error {
	tx.boundaries++
	return tx.Tx.Commit(ctx)
}
func (tx *atomicProbeTx) Rollback(ctx context.Context) error {
	tx.boundaries++
	return tx.Tx.Rollback(ctx)
}

func TestRouterAtomic_TransactionOwnership(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE atomic_router_probe (id integer)`); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"success", "warn", "error", "panic", "sql", "sql-warn", "sql-ignored"} {
		t.Run(outcome, func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			probe := &atomicProbeTx{Tx: tx}
			h := atomicProbeHandler{outcome}
			r := NewRouter(false)
			r.handlers[h.CompositeKey()] = h
			bc := BlockContext{Height: 42, TxIndex: 1, MsgIndex: 2, EventIndex: 3,
				Buf: buffers.New(), Dirty: readmodel.NewDirty()}
			he, fatal := r.DispatchAtomic(ctx, probe, bc, h.CompositeKey(), json.RawMessage(`{"probe":true}`))
			if fatal != nil || probe.boundaries != 0 {
				t.Fatalf("fatal=%v transaction boundaries=%d", fatal, probe.boundaries)
			}
			if outcome == "success" {
				if he != nil {
					t.Fatal(he)
				}
			} else {
				severity := SeverityError
				if outcome == "warn" {
					severity = SeverityWarn
				}
				if he == nil || he.Severity != severity || he.CompositeKey != h.CompositeKey() ||
					he.TxIndex == nil || *he.TxIndex != 1 || he.MsgIndex == nil || *he.MsgIndex != 2 ||
					he.EventIndex == nil || *he.EventIndex != 3 {
					t.Fatalf("lost failure context: %+v", he)
				}
				if outcome == "panic" && he.Stack == "" {
					t.Fatal("panic lost its stack")
				}
			}
			if outcome == "warn" {
				// A harmless warning allows the next handler to succeed.
				r.handlers[h.CompositeKey()] = atomicProbeHandler{"success"}
				if he, fatal = r.DispatchAtomic(ctx, probe, bc, h.CompositeKey(), nil); he != nil || fatal != nil {
					t.Fatalf("after warning: %+v, %v", he, fatal)
				}
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM atomic_router_probe`).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatalf("outer rollback left rows: %d, %v", remaining, err)
			}
		})
	}
}

func TestRouterIsolated_RestoresSQLAndBuffers(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE atomic_router_probe (id integer)`); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"error", "panic", "sql"} {
		t.Run(outcome, func(t *testing.T) {
			inTx(t, conn, func(tx pgx.Tx) {
				bc := BlockContext{Buf: buffers.New(), Dirty: readmodel.NewDirty()}
				bc.Buf.StatStructHealth = append(bc.Buf.StatStructHealth, buffers.StatRow{Value: "retained"})
				bc.Dirty.Player("1-retained")
				r := NewRouter(false)
				h := atomicProbeHandler{outcome}
				r.handlers[h.CompositeKey()] = h
				he, fatal := r.Dispatch(ctx, tx, bc, h.CompositeKey(), nil)
				if he == nil || he.Severity != SeverityError || fatal != nil {
					t.Fatalf("failure=%+v fatal=%v", he, fatal)
				}
				var remaining int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM atomic_router_probe`).Scan(&remaining); err != nil || remaining != 0 {
					t.Fatalf("savepoint failed to restore usable tx: %d, %v", remaining, err)
				}
				_, retained := bc.Dirty.Players["1-retained"]
				if len(bc.Buf.StatStructHealth) != 1 || bc.Buf.StatStructHealth[0].Value != "retained" || len(bc.Dirty.Players) != 1 || !retained {
					t.Fatal("failed handler leaked buffered rows or dirty keys")
				}
			})
		})
	}
}
