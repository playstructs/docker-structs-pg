package sync

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/readmodel"
	"sync-state/internal/rpc"
)

func (s *Syncer) reconcileCurrentBoundary(ctx context.Context, tx pgx.Tx, bundle *BlockBundle, dirty *readmodel.Dirty) error {
	cursor, err := db.ReadCursor(ctx, tx, bundle.ChainID)
	if err != nil {
		return err
	}
	if bundle.Height <= 0 || cursor.LastHeight != bundle.Height || cursor.LastBlockHash == "" ||
		!strings.EqualFold(cursor.LastBlockHash, bundle.BlockHashHex) || !cursor.LastBlockTime.Equal(bundle.BlockTime.Truncate(time.Microsecond)) {
		return fmt.Errorf("reconciliation requires the exact indexed cursor height, hash and time; refusing historical state overwrite")
	}
	ordered, err := rpc.OrderedEvents(bundle.FinalizeBlockEvents, bundle.TxResults)
	if err != nil {
		return err
	}
	_, err = events.ReconcileLegacyInfusionPositions(ctx, tx, events.BlockContext{
		ChainID: bundle.ChainID, Height: bundle.Height, BlockTime: bundle.BlockTime, Dirty: dirty,
	}, ordered)
	return err
}

// This command intentionally repairs only the current complete block. Replaying
// an arbitrary historical range over newer snapshots is not state restoration.
// Both locks are held through source validation, repair and projection rebuild.
func runReconcileCurrentBlock(ctx context.Context, cfg Config, stderr io.Writer) int {
	if cfg.StartHeight <= 0 {
		fmt.Fprintln(stderr, "reconcile-current-block requires -start=<exact indexed height>")
		return 2
	}
	client := rpc.NewClient(cfg.RPCURLs(), cfg.HTTPTimeout, cfg.HTTPMaxRetries)
	status, err := client.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "reconcile-current-block: RPC status: %v\n", err)
		return 1
	}
	chainID := status.NodeInfo.Network
	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, chainID)
	if err != nil {
		fmt.Fprintf(stderr, "reconcile-current-block: %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := db.CheckEconomySchema(ctx, pool.Pool); err != nil {
		fmt.Fprintf(stderr, "reconcile-current-block: schema: %v\n", err)
		return 1
	}
	s := NewSyncer(cfg, client, pool, events.NewRouter(cfg.StrictUnknownEvents), chainID)
	bundle, err := s.fetchBundle(ctx, cfg.StartHeight)
	if err != nil {
		fmt.Fprintf(stderr, "reconcile-current-block: source: %v\n", err)
		return 1
	}
	tx, err := pool.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "reconcile-current-block: begin: %v\n", err)
		return 1
	}
	defer tx.Rollback(ctx)
	result, err := s.applyBlockInTx(ctx, tx, bundle, status.Latest(), buffers.New(), applyOpts{ReconcileCurrentBlock: true})
	if err == nil {
		err = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
	if result != nil {
		var pending []pendingHandlerError
		for _, he := range result.PendingErrors {
			pending = append(pending, pendingHandlerError{height: bundle.Height, he: he})
		}
		s.writePendingHandlerErrors(ctx, chainID, pending)
	}
	if err != nil {
		fmt.Fprintf(stderr, "reconcile-current-block: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "Reconciled current block: chain=%s height=%d hash=%s; native snapshot readiness must still be checked before enabling writes.\n",
		chainID, bundle.Height, bundle.BlockHashHex)
	return 0
}
