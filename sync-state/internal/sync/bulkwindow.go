package sync

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/db"
	"sync-state/internal/events"
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
}

// blockApplyResult carries side effects that applyBlockInTx deliberately
// does not persist (handler_error_log is written post-commit).
type blockApplyResult struct {
	PendingErrors []events.HandlerError
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

	opts := applyOpts{
		SkipCurrentBlockHeartbeat: true,
		SkipCursorUpsert:          true,
		SkipStatementTimeout:      true,
	}

	var allPending []pendingHandlerError
	for _, bundle := range blocks {
		res, err := s.applyBlockInTx(ctx, tx, bundle, tipHeight, opts)
		if err != nil {
			return fmt.Errorf("bulk apply h=%d: %w", bundle.Height, err)
		}
		for _, he := range res.PendingErrors {
			allPending = append(allPending, pendingHandlerError{
				height: bundle.Height,
				he:     he,
			})
		}
	}

	last := blocks[len(blocks)-1]
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
