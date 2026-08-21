package readmodel

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BackfillReport struct {
	Height  int64
	Rows    map[string]int64
	Elapsed time.Duration
}

// NeedsBackfill returns true until every model has a refresh row at the
// current indexed height. A partial prior attempt is always rebuilt.
func NeedsBackfill(ctx context.Context, pool *pgxpool.Pool, height int64) (bool, error) {
	var count int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM structs.api_refresh_state
WHERE model = ANY($1::text[]) AND source_height = $2`, modelNames, height).Scan(&count); err != nil {
		return false, err
	}
	return count != len(modelNames), nil
}

func Backfill(ctx context.Context, pool *pgxpool.Pool, height int64, sourceTime *time.Time) (BackfillReport, error) {
	started := time.Now()
	report := BackfillReport{Height: height, Rows: map[string]int64{}}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return report, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	d := NewDirty()
	if err := loadAllDirty(ctx, tx, d); err != nil {
		return report, err
	}
	// Clearing first guarantees deletion of projections whose authoritative
	// entities disappeared before this deployment.
	for _, table := range []string{
		"api_leaderboard_player", "api_leaderboard_guild",
		"api_leaderboard_reactor", "api_leaderboard_provider",
		"api_leaderboard_substation", "api_inventory", "api_guild_bank",
	} {
		if _, err := tx.Exec(ctx, "DELETE FROM structs."+table); err != nil {
			return report, fmt.Errorf("clear %s: %w", table, err)
		}
	}
	if err := Recompute(ctx, tx, d, height, sourceTime); err != nil {
		return report, err
	}
	for _, table := range []string{
		"api_inventory", "api_guild_bank", "api_leaderboard_player",
		"api_leaderboard_guild", "api_leaderboard_reactor",
		"api_leaderboard_provider", "api_leaderboard_substation",
	} {
		var n int64
		if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM structs."+table).Scan(&n); err != nil {
			return report, fmt.Errorf("count %s: %w", table, err)
		}
		report.Rows[table] = n
	}
	if err := tx.Commit(ctx); err != nil {
		return report, fmt.Errorf("commit: %w", err)
	}
	report.Elapsed = time.Since(started)
	return report, nil
}

func loadAllDirty(ctx context.Context, tx pgx.Tx, d *Dirty) error {
	specs := []struct {
		query string
		add   func(string)
	}{
		{`SELECT DISTINCT address FROM structs.ledger`, d.Address},
		{`SELECT id FROM structs.player`, d.Player},
		{`SELECT id FROM structs.guild`, d.Guild},
		{`SELECT id FROM structs.reactor`, d.Reactor},
		{`SELECT id FROM structs.provider`, d.Provider},
		{`SELECT id FROM structs.substation`, d.Substation},
	}
	for _, spec := range specs {
		rows, err := tx.Query(ctx, spec.query)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			spec.add(id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
