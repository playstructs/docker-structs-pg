package sync

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/readmodel"
)

// applyOpts tweaks per-block writes when running inside a bulk outer tx.
type applyOpts struct {
	// SkipCurrentBlockHeartbeat defers pg_notify('grass', ...) to the
	// bulk window close so the webapp sees one pulse per window during
	// catch-up instead of one per block.
	SkipCurrentBlockHeartbeat bool
	// SkipCursorUpsert defers sync_state.sync_cursor to the bulk window
	// close. block_log is still written per block (audit trail).
	SkipCursorUpsert bool
	// SkipStatementTimeout skips SET LOCAL statement_timeout inside
	// applyBlockInTx. The bulk outer tx sets BulkStatementTimeout once.
	SkipStatementTimeout bool
	// DeferProjections skips readmodel.Recompute and returns the block's
	// dirty set and ledger deltas instead, so the bulk window can recompute
	// the api_* projections once for all of its blocks.
	DeferProjections bool
}

// blockApplyResult carries side effects that applyBlockInTx deliberately
// does not persist (handler_error_log is written post-commit). Dirty and
// LedgerDeltas are set only under DeferProjections.
type blockApplyResult struct {
	PendingErrors []events.HandlerError
	Dirty         *readmodel.Dirty
	LedgerDeltas  []buffers.LedgerDelta
}

// applyBulkWindow runs blocks in ascending order inside one outer PG
// transaction and commits once at the end. Event order, handler SAVEPOINTs,
// and per-row INSERT/UPSERT semantics are identical to streaming mode —
// only commit frequency and deferred cursor/heartbeat differ.
func (s *Syncer) applyBulkWindow(ctx context.Context, blocks []*BlockBundle, tipHeight int64) error {
	if len(blocks) == 0 {
		return nil
	}

	tx, err := s.pool.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("bulk begin tx h=%d: %w", blocks[0].Height, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	timeout := s.cfg.BulkStatementTimeout
	if timeout <= 0 {
		timeout = s.cfg.StatementTimeout
	}
	if timeout > 0 {
		ms := timeout.Milliseconds()
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			return fmt.Errorf("bulk set statement_timeout h=%d: %w", blocks[0].Height, err)
		}
	}
	// The cursor commits in this same transaction, so a crash can only lose
	// whole windows, which the next start replays from the cursor.
	if s.cfg.BulkAsyncCommit {
		if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
			return fmt.Errorf("bulk set synchronous_commit h=%d: %w", blocks[0].Height, err)
		}
	}

	opts := applyOpts{
		SkipCurrentBlockHeartbeat: true,
		SkipCursorUpsert:          true,
		SkipStatementTimeout:      true,
		DeferProjections:          true,
	}

	// Reuse one buffer; applyBlockInTx flushes it after every block. The
	// api_* projections are pure functions of authoritative state (address
	// inventory deltas are additive), so recomputing the union of the
	// window's dirty sets once gives the same rows as recomputing per block.
	buf := buffers.New()
	windowDirty := readmodel.NewDirty()
	var windowDeltas []buffers.LedgerDelta

	var allPending []pendingHandlerError
	for _, bundle := range blocks {
		res, err := s.applyBlockInTx(ctx, tx, bundle, tipHeight, buf, opts)
		if err != nil {
			return fmt.Errorf("bulk apply h=%d: %w", bundle.Height, err)
		}
		if !s.cfg.BulkDeferProjections {
			windowDirty.Merge(res.Dirty)
			windowDeltas = append(windowDeltas, res.LedgerDeltas...)
		}
		for _, he := range res.PendingErrors {
			allPending = append(allPending, pendingHandlerError{
				height: bundle.Height,
				he:     he,
			})
		}
	}

	last := blocks[len(blocks)-1]
	if !s.cfg.BulkDeferProjections {
		if err := readmodel.Recompute(ctx, tx, windowDirty, last.Height, last.BlockTime, readmodel.InventoryFromDeltas(windowDeltas)); err != nil {
			return fmt.Errorf("bulk api projections h=%d..%d: %w", blocks[0].Height, last.Height, err)
		}
	}
	status := db.ComputeStatus(last.Height, tipHeight)
	lag := tipHeight - last.Height
	if lag < 0 {
		lag = 0
	}
	if err := db.UpsertCurrentBlock(ctx, tx, db.CurrentBlockUpsert{
		Chain:     last.ChainID,
		Height:    last.Height,
		UpdatedAt: last.BlockTime,
		Status:    status,
		LagBlocks: lag,
		TipHeight: tipHeight,
	}); err != nil {
		return fmt.Errorf("bulk upsert current_block h=%d: %w", last.Height, err)
	}
	if err := db.EmitCurrentBlockHeartbeat(ctx, tx, last.Height, last.BlockTime); err != nil {
		return fmt.Errorf("bulk notify grass h=%d: %w", last.Height, err)
	}
	if err := db.UpsertCursor(ctx, tx, db.Cursor{
		ChainID:       last.ChainID,
		LastHeight:    last.Height,
		LastBlockHash: last.BlockHashHex,
		LastBlockTime: last.BlockTime,
		Status:        status,
		LagBlocks:     lag,
		TipHeight:     tipHeight,
	}); err != nil {
		return fmt.Errorf("bulk sync_cursor h=%d: %w", last.Height, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("bulk commit h=%d..%d: %w", blocks[0].Height, last.Height, err)
	}
	committed = true
	if s.cfg.BulkDeferProjections {
		s.projectionsStale = true
	}

	s.writePendingHandlerErrors(ctx, last.ChainID, allPending)
	return nil
}

type pendingHandlerError struct {
	height int64
	he     events.HandlerError
}

func (s *Syncer) writePendingHandlerErrors(ctx context.Context, chainID string, pending []pendingHandlerError) {
	for _, pe := range pending {
		he := pe.he
		if err := db.WriteHandlerError(ctx, s.pool.Pool, db.HandlerError{
			ChainID:      chainID,
			Height:       pe.height,
			TxIndex:      he.TxIndex,
			MsgIndex:     he.MsgIndex,
			EventIndex:   he.EventIndex,
			CompositeKey: he.CompositeKey,
			Payload:      he.Payload,
			Error:        he.Error,
			Severity:     he.Severity,
			Stack:        he.Stack,
		}); err != nil {
			fmt.Fprintf(s.logger.Writer(), "WARN: write handler_error_log h=%d ck=%s: %v\n", pe.height, he.CompositeKey, err)
		}
	}
}
