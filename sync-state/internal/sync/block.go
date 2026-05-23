package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/bank"
	"sync-state/internal/buffers"
	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/rpc"
)

// BlockBundle is everything we need to ingest one block: the /block and
// /block_results results plus convenience fields the writer reuses.
type BlockBundle struct {
	Height              int64
	ChainID             string
	BlockTime           time.Time
	BlockHashHex        string
	ProposerHex         string
	RawTxs              []string    // base64 from /block.block.data.txs
	TxResults           []rpc.TxResult
	FinalizeBlockEvents []rpc.Event
}

// applyBlock writes everything for one block in a single PG transaction:
//
//  1. dispatch each finalize_block + per-tx event to the events.Router; per-
//     handler errors collect into pendingErrors (block still commits)
//  2. bank ledger derivation (bank/staking events -> structs.ledger,
//     structs.defusion) — Go port of cache.PROCESS_BLOCK_LEDGER
//  3. mirror raw rows to sync_state.raw_* if -mirror-raw is set
//  4. UPSERT structs.current_block with the new status/lag/tip
//  5. emit pg_notify('grass', ...) directly so the webapp gets a heartbeat
//     regardless of whether structs.current_block actually changed (the
//     GRASS trigger guards against no-op UPDATEs)
//  6. write sync_state.block_log
//  7. bump sync_state.sync_cursor
//  8. commit
//
// After commit, pending handler-error rows are written to
// sync_state.handler_error_log (outside the per-block tx so they survive
// even when the block tx itself rolled back).
func (s *Syncer) applyBlock(ctx context.Context, bundle *BlockBundle, tipHeight int64) error {
	tx, err := s.pool.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx h=%d: %w", bundle.Height, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	buf := buffers.New()
	res, err := s.applyBlockInTx(ctx, tx, bundle, tipHeight, buf, applyOpts{})
	if err != nil {
		return err
	}
	if err := buf.Flush(ctx, tx); err != nil {
		return fmt.Errorf("flush buffer h=%d: %w", bundle.Height, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit h=%d: %w", bundle.Height, err)
	}
	committed = true

	pending := make([]pendingHandlerError, len(res.PendingErrors))
	for i, he := range res.PendingErrors {
		pending[i] = pendingHandlerError{height: bundle.Height, he: he}
	}
	s.writePendingHandlerErrors(ctx, bundle.ChainID, pending)
	return nil
}

// applyBlockInTx writes everything for one block inside an existing tx.
// See applyBlock for the full step list. Handler errors are returned for
// the caller to persist after commit.
//
// buf is the per-block (streaming) or per-window (bulk) append-only row
// buffer. Event handlers and bank.ProcessBlock push rows into buf;
// callers are responsible for invoking buf.Flush(ctx, tx) before commit.
func (s *Syncer) applyBlockInTx(ctx context.Context, tx pgx.Tx, bundle *BlockBundle, tipHeight int64, buf *buffers.Buffer, opts applyOpts) (*blockApplyResult, error) {
	if !opts.SkipStatementTimeout && s.cfg.StatementTimeout > 0 {
		ms := s.cfg.StatementTimeout.Milliseconds()
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			return nil, fmt.Errorf("set statement_timeout h=%d: %w", bundle.Height, err)
		}
	}

	pendingErrs := make([]events.HandlerError, 0)
	numEvents := 0
	if err := s.dispatchEvents(ctx, tx, bundle, buf, &pendingErrs, &numEvents); err != nil {
		return nil, fmt.Errorf("dispatch h=%d: %w", bundle.Height, err)
	}

	// Snapshot the buffer around the bank savepoint so a bank
	// ProcessBlock failure truncates the rows it appended before
	// returning. Mirrors the per-handler snapshot/restore in
	// events.Router.Dispatch.
	bankBufSnap := buf.Snapshot()
	if bankSP, bankErr := tx.Begin(ctx); bankErr != nil {
		pendingErrs = append(pendingErrs, events.HandlerError{
			CompositeKey: "bank.process_block",
			Error:        fmt.Sprintf("savepoint begin: %v", bankErr),
			Severity:     events.SeverityError,
		})
	} else if err := bank.ProcessBlock(ctx, bankSP, buf, bundle.Height, bundle.BlockTime, bundle.FinalizeBlockEvents, bundle.TxResults); err != nil {
		_ = bankSP.Rollback(ctx)
		buf.Restore(bankBufSnap)
		pendingErrs = append(pendingErrs, events.HandlerError{
			CompositeKey: "bank.process_block",
			Error:        err.Error(),
			Severity:     events.SeverityError,
		})
	} else if err := bankSP.Commit(ctx); err != nil {
		pendingErrs = append(pendingErrs, events.HandlerError{
			CompositeKey: "bank.process_block",
			Error:        fmt.Sprintf("savepoint commit: %v", err),
			Severity:     events.SeverityError,
		})
	}

	if s.cfg.MirrorRaw {
		if err := s.writeRawMirror(ctx, tx, bundle); err != nil {
			return nil, fmt.Errorf("raw mirror h=%d: %w", bundle.Height, err)
		}
	}

	status := db.ComputeStatus(bundle.Height, tipHeight)
	lag := tipHeight - bundle.Height
	if lag < 0 {
		lag = 0
	}

	if !opts.SkipCurrentBlockHeartbeat || !opts.SkipCursorUpsert {
		if err := db.UpsertCurrentBlock(ctx, tx, db.CurrentBlockUpsert{
			Chain:     bundle.ChainID,
			Height:    bundle.Height,
			UpdatedAt: bundle.BlockTime,
			Status:    status,
			LagBlocks: lag,
			TipHeight: tipHeight,
		}); err != nil {
			return nil, fmt.Errorf("upsert current_block h=%d: %w", bundle.Height, err)
		}
	}

	if !opts.SkipCurrentBlockHeartbeat {
		if err := db.EmitCurrentBlockHeartbeat(ctx, tx, bundle.Height, bundle.BlockTime); err != nil {
			return nil, fmt.Errorf("notify grass h=%d: %w", bundle.Height, err)
		}
	}

	if err := db.WriteBlockLog(ctx, tx, db.BlockLogEntry{
		ChainID:          bundle.ChainID,
		Height:           bundle.Height,
		BlockHash:        bundle.BlockHashHex,
		BlockTime:        bundle.BlockTime,
		NumTxs:           len(bundle.TxResults),
		NumEvents:        numEvents,
		NumHandlerErrors: len(pendingErrs),
	}); err != nil {
		return nil, fmt.Errorf("block_log h=%d: %w", bundle.Height, err)
	}

	if !opts.SkipCursorUpsert {
		if err := db.UpsertCursor(ctx, tx, db.Cursor{
			ChainID:       bundle.ChainID,
			LastHeight:    bundle.Height,
			LastBlockHash: bundle.BlockHashHex,
			LastBlockTime: bundle.BlockTime,
			Status:        status,
			LagBlocks:     lag,
			TipHeight:     tipHeight,
		}); err != nil {
			return nil, fmt.Errorf("sync_cursor h=%d: %w", bundle.Height, err)
		}
	}

	return &blockApplyResult{PendingErrors: pendingErrs}, nil
}

// dispatchEvents walks finalize_block + per-tx events in deterministic order
// and calls Router.Dispatch on each one. Composite keys are
// "<event.Type>.<attribute.Key>" per cache.attributes' historical encoding.
//
// Within an event we walk attributes in attribute order, dedup'd by key
// (keep first occurrence) to match cache.attributes's UNIQUE constraint.
func (s *Syncer) dispatchEvents(ctx context.Context, tx pgx.Tx, b *BlockBundle, buf *buffers.Buffer, errs *[]events.HandlerError, numEvents *int) error {
	bctx := events.BlockContext{
		ChainID:   b.ChainID,
		Height:    b.Height,
		BlockTime: b.BlockTime,
		TipHeight: b.Height, // applyBlock tracks tipHeight separately; handlers don't currently need it
		Buf:       buf,
	}

	// Finalize-block events (BeginBlock/EndBlock): tx_index = -1.
	for evIdx, ev := range b.FinalizeBlockEvents {
		bctx.TxIndex = -1
		bctx.MsgIndex = -1
		bctx.EventIndex = evIdx
		if err := s.dispatchOneEvent(ctx, tx, bctx, ev, errs); err != nil {
			return err
		}
		*numEvents++
	}

	// Per-tx events: tx_index = index, ordered by attribute.
	for txIdx, tr := range b.TxResults {
		for evIdx, ev := range tr.Events {
			bctx.TxIndex = txIdx
			bctx.MsgIndex = -1
			bctx.EventIndex = evIdx
			if err := s.dispatchOneEvent(ctx, tx, bctx, ev, errs); err != nil {
				return err
			}
			*numEvents++
		}
	}
	return nil
}

func (s *Syncer) dispatchOneEvent(ctx context.Context, tx pgx.Tx, bctx events.BlockContext, ev rpc.Event, errs *[]events.HandlerError) error {
	seen := make(map[string]struct{}, len(ev.Attributes))
	for _, a := range ev.Attributes {
		if _, dup := seen[a.Key]; dup {
			continue
		}
		seen[a.Key] = struct{}{}
		compositeKey := ev.Type + "." + a.Key
		raw := encodeAttributeValue(a.Value)

		he, fatal := s.router.Dispatch(ctx, tx, bctx, compositeKey, raw)
		if fatal != nil {
			return fatal // strict-unknown-events
		}
		if he != nil {
			*errs = append(*errs, *he)
		}
	}
	return nil
}

// encodeAttributeValue turns an attribute Value (always a string in the
// CometBFT wire format) into a json.RawMessage. If the string already looks
// like a JSON object/array, we pass it through; otherwise we wrap it as a
// JSON string literal.
func encodeAttributeValue(v string) json.RawMessage {
	t := stripWhitespace(v)
	if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		return json.RawMessage(v)
	}
	enc, _ := json.Marshal(v)
	return enc
}

func stripWhitespace(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return s[i:]
		}
	}
	return ""
}

func (s *Syncer) writeRawMirror(ctx context.Context, tx pgx.Tx, b *BlockBundle) error {
	if err := db.WriteRawBlock(ctx, tx, db.RawMirrorBlock{
		ChainID:   b.ChainID,
		Height:    b.Height,
		BlockHash: b.BlockHashHex,
		BlockTime: b.BlockTime,
		Proposer:  b.ProposerHex,
		NumTxs:    len(b.RawTxs),
	}); err != nil {
		return err
	}

	// tx_results
	trRows := make([]db.RawMirrorTxResult, 0, len(b.TxResults))
	for i, tr := range b.TxResults {
		var rawTxBytes []byte
		if i < len(b.RawTxs) {
			rawTxBytes, _ = base64.StdEncoding.DecodeString(b.RawTxs[i])
		}
		trJSON, _ := json.Marshal(tr)
		gas, _ := strconv.ParseInt(tr.GasUsed, 10, 64)
		var gasPtr *int64
		if gas > 0 {
			gasPtr = &gas
		}
		trRows = append(trRows, db.RawMirrorTxResult{
			ChainID: b.ChainID,
			Height:  b.Height,
			TxIndex: i,
			TxHash:  TxHash(rawTxBytes),
			Code:    tr.Code,
			GasUsed: gasPtr,
			Log:     tr.Log,
			RawJSON: trJSON,
		})
	}
	if err := db.WriteRawTxResults(ctx, tx, trRows); err != nil {
		return err
	}

	// events + attributes
	var evRows []db.RawMirrorEvent
	var attrRows []db.RawMirrorAttribute
	addEv := func(txIdx *int, evIdx int, ev rpc.Event) {
		evRows = append(evRows, db.RawMirrorEvent{
			ChainID:    b.ChainID,
			Height:     b.Height,
			TxIndex:    txIdx,
			EventIndex: evIdx,
			EventType:  ev.Type,
		})
		seen := make(map[string]struct{}, len(ev.Attributes))
		for _, a := range ev.Attributes {
			if _, dup := seen[a.Key]; dup {
				continue
			}
			seen[a.Key] = struct{}{}
			attrRows = append(attrRows, db.RawMirrorAttribute{
				ChainID:      b.ChainID,
				Height:       b.Height,
				TxIndex:      txIdx,
				EventIndex:   evIdx,
				Key:          a.Key,
				Value:        a.Value,
				CompositeKey: ev.Type + "." + a.Key,
			})
		}
	}
	for evIdx, ev := range b.FinalizeBlockEvents {
		addEv(nil, evIdx, ev)
	}
	for i, tr := range b.TxResults {
		idx := i
		for evIdx, ev := range tr.Events {
			addEv(&idx, evIdx, ev)
		}
	}
	if err := db.WriteRawEvents(ctx, tx, evRows); err != nil {
		return err
	}
	if err := db.WriteRawAttributes(ctx, tx, attrRows); err != nil {
		return err
	}
	return nil
}

// validateBundle catches obvious shape problems before we try to write.
func validateBundle(b *rpc.BlockResult, br *rpc.BlockResultsResult, chainID string, height int64) error {
	if b.Block.Header.ChainID != chainID {
		return fmt.Errorf("block %d chain_id=%q != live %q", height, b.Block.Header.ChainID, chainID)
	}
	gotH, _ := strconv.ParseInt(b.Block.Header.Height, 10, 64)
	if gotH != height {
		return fmt.Errorf("block %d returned header.height=%d", height, gotH)
	}
	_ = br
	return nil
}
