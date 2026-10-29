package sync

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sync-state/internal/lcd"
	"sync-state/internal/readmodel"
	"sync-state/internal/staking"
)

// The worker belongs to the existing single writer. It never holds a SQL lock
// while fetching HTTP pages and cannot postpone block ingestion on LCD failure.
// Shutdown cancels and joins it before the pool can be closed.
func (s *Syncer) startNativeRefresh(ctx context.Context) func() {
	if !s.cfg.NativeStakingRefresh || s.cfg.LCDURL == "" {
		return func() {}
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		seeded := false
		for workerCtx.Err() == nil {
			if !seeded {
				var err error
				seeded, err = s.seedNativeCandidates(workerCtx)
				if err != nil && workerCtx.Err() == nil {
					s.logger.Printf("native staking candidate seed: %v", err)
				}
			}
			if seeded {
				if _, err := s.refreshNativeBatch(workerCtx); err != nil && workerCtx.Err() == nil {
					s.logger.Printf("native staking refresh: %v", err)
				}
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-workerCtx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

// Startup discovers existing owned/observed positions once. Inventory provides
// candidate addresses only, never the claim values. Live events add later ones.
func (s *Syncer) seedNativeCandidates(ctx context.Context) (bool, error) {
	tx, err := s.pool.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := readmodel.Lock(ctx, tx); err != nil {
		return false, err
	}
	var height int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT last_height FROM sync_state.sync_cursor WHERE chain_id=$1),0)`, s.chainID).Scan(&height); err != nil {
		return false, err
	}
	if height <= 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
WITH candidates AS (
  SELECT address FROM structs.asset_owner
  UNION SELECT address FROM structs.infusion WHERE destination_type='reactor'
  UNION SELECT owner_id FROM structs.api_inventory
        WHERE owner_type='address' AND denom IN ('ualpha.infused','ualpha.defusing')
)
INSERT INTO sync_state.native_staking_snapshot(chain_id,address,required_height)
SELECT $1,address,$2 FROM candidates
WHERE address ~ '^structs1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]{1,121}$'
ON CONFLICT (chain_id,address) DO NOTHING`, s.chainID, height); err != nil {
		return false, err
	}
	if err := staking.SeedReceipts(ctx, tx, s.chainID, height); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// One batch is bounded by both count and wall time. Failed addresses back off;
// a newer invalidation removes that delay. These are query-work bounds, not
// transaction/withdrawal limits or a claim that truncated positions are valid.
func (s *Syncer) refreshNativeBatch(ctx context.Context) (int, error) {
	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	items, err := staking.Pending(batchCtx, s.pool.Pool, s.chainID, 8)
	if err != nil {
		return 0, err
	}
	client := lcd.Client{BaseURL: s.cfg.LCDURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
	limits := staking.Limits{PageSize: 100, MaxPairs: 1024, MaxEntries: 8192}
	published := 0
	for _, item := range items {
		if batchCtx.Err() != nil {
			break
		}
		snapshot, fetchErr := staking.Fetch(batchCtx, client, s.chainID, item.Address, item.Height, limits)
		if ctx.Err() != nil {
			return published, ctx.Err()
		}
		// Use the parent after a fetch deadline to record the retry. SQL has
		// its own bound, so a busy maintenance lock cannot strand the worker.
		writeCtx, cancelWrite := context.WithTimeout(ctx, 5*time.Second)
		accepted := false
		err := func() error {
			tx, err := s.pool.Pool.Begin(writeCtx)
			if err != nil {
				return err
			}
			defer tx.Rollback(writeCtx)
			if fetchErr != nil {
				if err := staking.RetryLater(writeCtx, tx, s.chainID, item.Address, item.Height, time.Now().Add(30*time.Second)); err != nil {
					return err
				}
			} else {
				ok, err := staking.Publish(writeCtx, tx, snapshot)
				if err != nil {
					return err
				}
				if ok {
					if err := readmodel.RefreshNativeClaims(writeCtx, tx, item.Address); err != nil {
						return err
					}
				}
				accepted = ok
			}
			return tx.Commit(writeCtx)
		}()
		cancelWrite()
		if err != nil {
			return published, fmt.Errorf("publish address %s at %d: %w", item.Address, item.Height, err)
		}
		if accepted {
			published++
		}
		if fetchErr != nil {
			s.logger.Printf("native staking unknown: address=%s height=%d: %v", item.Address, item.Height, fetchErr)
		}
	}
	return published, nil
}
