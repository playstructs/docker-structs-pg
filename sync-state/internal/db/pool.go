// Package db owns the Postgres surface for sync-state: the connection pool,
// the writer advisory lock, the sync_state.* bookkeeping schema, and the
// per-block transactional writers.
package db

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps pgxpool.Pool plus a dedicated long-lived connection that holds
// the writer advisory lock for the chain we're indexing. The lock is released
// automatically when the connection closes (Close).
type Pool struct {
	Pool     *pgxpool.Pool
	lockConn *pgx.Conn
	lockKey  string
	lockID   int64
}

// New opens a pool and acquires the writer lock for chainID. Second sync-state
// instance pointing at the same DB + chainID fails fast with ErrWriterLocked.
//
// maxConns of 0 leaves pgx's default (max(4, NumCPU())).
func New(ctx context.Context, dsn string, maxConns int, chainID string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	lockConn, lockID, err := acquireWriterLock(ctx, dsn, chainID)
	if err != nil {
		pool.Close()
		return nil, err
	}

	return &Pool{
		Pool:     pool,
		lockConn: lockConn,
		lockKey:  "sync-state-" + chainID,
		lockID:   lockID,
	}, nil
}

// Close releases the writer lock and shuts the pool down.
func (p *Pool) Close() {
	if p.lockConn != nil {
		ctx, cancel := closeCtx()
		defer cancel()
		_ = p.lockConn.Close(ctx)
	}
	if p.Pool != nil {
		p.Pool.Close()
	}
}

// LockKey returns the human-readable lock key (for logging).
func (p *Pool) LockKey() string { return p.lockKey }

// LockID returns the numeric key used for pg_advisory_lock.
func (p *Pool) LockID() int64 { return p.lockID }

// ErrWriterLocked is returned when another sync-state instance already holds
// the writer lock for the same chainID.
var ErrWriterLocked = fmt.Errorf("another sync-state already holds the writer lock")

func acquireWriterLock(ctx context.Context, dsn, chainID string) (*pgx.Conn, int64, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, 0, fmt.Errorf("open lock conn: %w", err)
	}
	lockID := writerLockID(chainID)
	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, lockID,
	).Scan(&acquired); err != nil {
		_ = conn.Close(ctx)
		return nil, 0, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !acquired {
		_ = conn.Close(ctx)
		return nil, 0, fmt.Errorf("%w for chain %s (lock_id=%d)", ErrWriterLocked, chainID, lockID)
	}
	return conn, lockID, nil
}

// writerLockID derives a stable int64 from "sync-state-<chain_id>" so two
// sync-state binaries acting on the same chain collide on the same advisory
// lock key.
func writerLockID(chainID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("sync-state-"))
	_, _ = h.Write([]byte(chainID))
	return int64(h.Sum64()) // pg_advisory_lock accepts any int8
}
