package backfill

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/db"
)

type snapshotBoundary struct {
	chainID string
	height  int64
	time    time.Time
}

func readBoundary(ctx context.Context, pool *pgxpool.Pool, chainID string) (snapshotBoundary, error) {
	if chainID == "" {
		return snapshotBoundary{}, fmt.Errorf("backfill requires the indexed chain ID")
	}
	cursor, err := db.ReadCursor(ctx, pool, chainID)
	if err != nil {
		return snapshotBoundary{}, err
	}
	if cursor.LastHeight > 0 && cursor.LastBlockTime.IsZero() {
		return snapshotBoundary{}, fmt.Errorf("backfill cursor has no committed block time")
	}
	return snapshotBoundary{chainID, cursor.LastHeight, cursor.LastBlockTime}, nil
}

// check runs after the maintenance lock and before publishing fetched rows.
// The snapshot is discarded if ingestion advanced while it was being fetched.
func (b snapshotBoundary) check(ctx context.Context, tx pgx.Tx) error {
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_state.sync_cursor
WHERE chain_id=$1 AND last_height=$2 AND last_block_time=$3)`, b.chainID, b.height, b.time).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("backfill cursor changed during snapshot fetch at height %d; retry", b.height)
	}
	return nil
}

func validObjectID(id, prefix string) bool {
	parts := strings.Split(id, "-")
	if len(parts) != 2 || parts[0] != prefix {
		return false
	}
	index, err := strconv.ParseUint(parts[1], 10, 64)
	return err == nil && index != 0 && strconv.FormatUint(index, 10) == parts[1]
}
