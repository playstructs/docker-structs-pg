package doctor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"sync-state/internal/influence"
)

func probeInfluence(ctx context.Context, pool *pgxpool.Pool, chain string) Check {
	c := Check{Name: "influence", Severity: OK}
	if err := influence.ValidateSchema(ctx, pool); err != nil {
		c.Severity = FATAL
		c.Detail = err.Error()
		return c
	}
	var start, h, base int64
	var unresolved int
	err := pool.QueryRow(ctx, `SELECT i.coverage_start,i.height,coalesce(b.height,i.height),
 (SELECT count(*) FROM structs.influence_published_build p JOIN structs.influence_projection_build x USING(build_id)
 WHERE x.chain_id=i.chain_id AND EXISTS(SELECT 1 FROM structs.influence_contest_planet cp WHERE cp.build_id=x.build_id AND cp.sector=0))
 FROM structs.influence_source_cursor i LEFT JOIN structs.current_block b ON b.chain=i.chain_id WHERE i.chain_id=$1`, chain).Scan(&start, &h, &base, &unresolved)
	if err == pgx.ErrNoRows {
		c.Detail = "schema ready; exact coverage begins at the next verified block boundary"
		return c
	}
	if err != nil {
		c.Severity = FATAL
		c.Detail = err.Error()
		return c
	}
	c.Detail = fmt.Sprintf("coverage=[%d,%d], canonical_height=%d, builds_with_unassigned_sectors=%d", start, h, base, unresolved)
	if h != base {
		c.Severity = FATAL
	}
	if unresolved > 0 && c.Severity != FATAL {
		c.Severity = WARN
	}
	return c
}
