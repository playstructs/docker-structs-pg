package events

import "sync-state/internal/buffers"

// appendLedger stamps source-event identity onto each row (leg 0..n-1
// within this event) and appends them to the per-block buffer. Time and
// height always come from bctx so replay lands in the correct chunk.
func appendLedger(bctx BlockContext, rows ...buffers.LedgerRow) {
	if bctx.Buf == nil {
		return
	}
	for i, row := range rows {
		row.ChainID = bctx.ChainID
		row.TxIndex = bctx.TxIndex
		row.MsgIndex = bctx.MsgIndex
		row.EventIndex = buffers.EncodeLedgerEventIndex(bctx.EventIndex, i)
		row.BlockHeight = bctx.Height
		row.Time = bctx.BlockTime.UTC()
		bctx.Buf.Ledger = append(bctx.Buf.Ledger, row)
	}
}
