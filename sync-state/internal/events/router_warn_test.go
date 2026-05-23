// Router-level coverage for the SeverityWarn path: when a registered
// handler returns ErrSkipWithWarn, Dispatch must release the SAVEPOINT
// without rolling back, return a HandlerError with Severity="warn", and
// leave the outer transaction usable so subsequent handlers in the same
// block still run.
//
// We use the real gridHandler because it's the first handler that emits
// ErrSkipWithWarn in production (genesis `attributeId="2-"`). Tests share
// the same INTEGRATION_DATABASE_URL guard as the rest of the events
// package's integration tests.
package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
)

func TestRouter_Dispatch_WarnSentinelKeepsTxOpen(t *testing.T) {
	conn := connect(t)
	r := NewRouter(false)
	ck := (gridHandler{}).CompositeKey()

	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()

		buf := buffers.New()
		// Malformed attributeId -> ErrSkipWithWarn -> Severity="warn".
		raw, _ := json.Marshal(map[string]any{"attributeId": "2-", "value": "1"})
		he, fatal := r.Dispatch(ctx, tx, BlockContext{ChainID: "test", Height: 1, TxIndex: -1, MsgIndex: -1, EventIndex: 0, Buf: buf}, ck, raw)
		if fatal != nil {
			t.Fatalf("warn dispatch returned fatal: %v", fatal)
		}
		if he == nil {
			t.Fatalf("warn dispatch returned no HandlerError")
		}
		if he.Severity != SeverityWarn {
			t.Errorf("severity = %q want %q", he.Severity, SeverityWarn)
		}

		// Outer tx is still usable: a follow-up well-formed grid event
		// should land normally without "current transaction is aborted".
		raw2, _ := json.Marshal(map[string]any{"attributeId": "0-5-99999", "value": "42"})
		he2, fatal2 := r.Dispatch(ctx, tx, BlockContext{ChainID: "test", Height: 1, TxIndex: -1, MsgIndex: -1, EventIndex: 1, Buf: buf}, ck, raw2)
		if fatal2 != nil {
			t.Fatalf("post-warn dispatch returned fatal: %v", fatal2)
		}
		if he2 != nil {
			t.Errorf("post-warn dispatch unexpected HandlerError: %+v", he2)
		}
	})
}

func TestRouter_Dispatch_HardErrorRollsBackSavepoint(t *testing.T) {
	conn := connect(t)
	r := NewRouter(false)
	ck := (gridHandler{}).CompositeKey()

	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()

		buf := buffers.New()
		// Non-numeric attributeId is a real chain bug — handler returns
		// a plain error (NOT ErrSkipWithWarn) so severity must be
		// "error" and the savepoint rolled back.
		raw, _ := json.Marshal(map[string]any{"attributeId": "abc-1-2", "value": "1"})
		he, fatal := r.Dispatch(ctx, tx, BlockContext{ChainID: "test", Height: 1, TxIndex: -1, MsgIndex: -1, EventIndex: 0, Buf: buf}, ck, raw)
		if fatal != nil {
			t.Fatalf("error dispatch returned fatal: %v", fatal)
		}
		if he == nil {
			t.Fatalf("error dispatch returned no HandlerError")
		}
		if he.Severity != SeverityError {
			t.Errorf("severity = %q want %q", he.Severity, SeverityError)
		}

		// Outer tx is still usable post-rollback (the whole point of
		// the SAVEPOINT — proven by another dispatch landing cleanly).
		raw2, _ := json.Marshal(map[string]any{"attributeId": "0-5-99998", "value": "100"})
		he2, fatal2 := r.Dispatch(ctx, tx, BlockContext{ChainID: "test", Height: 1, TxIndex: -1, MsgIndex: -1, EventIndex: 1, Buf: buf}, ck, raw2)
		if fatal2 != nil || he2 != nil {
			t.Errorf("post-error dispatch should be clean: fatal=%v he=%+v", fatal2, he2)
		}
	})
}
