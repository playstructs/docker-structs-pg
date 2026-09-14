package events

import (
	"testing"
	"time"

	"sync-state/internal/buffers"
	"sync-state/internal/readmodel"
)

func TestAppendLedgerStampsIdentityAndLegs(t *testing.T) {
	buf := buffers.New()
	bctx := BlockContext{
		ChainID:    "structs-1",
		Height:     42,
		BlockTime:  time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		TxIndex:    3,
		MsgIndex:   -1,
		EventIndex: 5,
		Buf:        buf,
		Dirty:      readmodel.NewDirty(),
	}
	appendLedger(bctx,
		buffers.LedgerRow{Address: "a", AmountP: "1", Action: "sent", Direction: "debit", Denom: "ualpha"},
		buffers.LedgerRow{Address: "b", AmountP: "1", Action: "received", Direction: "credit", Denom: "ualpha"},
	)
	if len(buf.Ledger) != 2 {
		t.Fatalf("rows=%d", len(buf.Ledger))
	}
	if buf.Ledger[0].ChainID != "structs-1" || buf.Ledger[0].TxIndex != 3 || buf.Ledger[0].MsgIndex != -1 {
		t.Fatalf("identity: %+v", buf.Ledger[0])
	}
	if buf.Ledger[0].EventIndex != buffers.EncodeLedgerEventIndex(5, 0) {
		t.Fatalf("leg0 event_index=%d", buf.Ledger[0].EventIndex)
	}
	if buf.Ledger[1].EventIndex != buffers.EncodeLedgerEventIndex(5, 1) {
		t.Fatalf("leg1 event_index=%d", buf.Ledger[1].EventIndex)
	}
	if buf.Ledger[0].Time != bctx.BlockTime || buf.Ledger[0].BlockHeight != 42 {
		t.Fatalf("time/height: %+v", buf.Ledger[0])
	}
}
