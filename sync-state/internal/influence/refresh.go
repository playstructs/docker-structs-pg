package influence

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// Refresh advances at most 10,000 blocks per published build per invocation.
// Use a dedicated connection as the Timescale view owner: the session lock
// must survive CALL's transaction boundary and must never leak into a pool.
// Rebuilds use new build IDs; a build ID is also its immutable source revision.
func Refresh(ctx context.Context, conn *pgx.Conn, out io.Writer) error {
	if err := ValidateSchema(ctx, conn); err != nil {
		return err
	}
	rows, err := conn.Query(ctx, `SELECT build_id FROM structs.influence_published_build ORDER BY build_id`)
	if err != nil {
		return err
	}
	var builds []int64
	for rows.Next() {
		var b int64
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return err
		}
		builds = append(builds, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range builds {
		if err := refreshBuild(ctx, conn, b, out); err != nil {
			return fmt.Errorf("influence refresh build %d: %w", b, err)
		}
	}
	return nil
}

func refreshBuild(ctx context.Context, conn *pgx.Conn, b int64, out io.Writer) error {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('structs.influence.writer',0))`); err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(c, `SELECT pg_advisory_unlock(hashtextextended('structs.influence.writer',0))`)
	}()
	var start, end int64
	err := conn.QueryRow(ctx, `SELECT coalesce(r.complete_before_height,c.start_height/1000*1000),
 least((x.projected_height+1)/1000*1000,coalesce(r.complete_before_height,c.start_height/1000*1000)+10000)
 FROM structs.influence_published_build p JOIN structs.influence_projection_build x USING(build_id)
 JOIN structs.influence_contest c ON c.contest_id=p.contest_id
 LEFT JOIN structs.influence_rollup_coverage r USING(build_id)
 WHERE p.build_id=$1 AND x.state IN ('live','final')`, b).Scan(&start, &end)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if end <= start {
		return nil
	}
	// Never mark a partial refresh as available. Failure/crash here leaves the
	// previous marker intact, so readers continue using raw deltas for the tail.
	_, err = conn.Exec(ctx, `CALL public.refresh_continuous_aggregate('structs.influence_score_change_1000', $1::bigint,$2::bigint, force=>true, options=>' {"buckets_per_batch":0}'::jsonb)`, start, end)
	if err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO structs.influence_checkpoint(build_id,height,guild,sector,unique_planets,held_planets)
 SELECT $1,$3-1,guild,sector,sum(u)::bigint,sum(c)::bigint FROM (
 SELECT guild,sector,unique_planets u,held_planets c FROM structs.influence_checkpoint WHERE build_id=$1 AND height=$2-1
 UNION ALL SELECT guild,sector,unique_delta,claim_delta FROM structs.influence_score_change_1000
 WHERE build_id=$1 AND bucket >= $2 AND bucket < $3
 ) d GROUP BY guild,sector ON CONFLICT(build_id,guild,sector,height) DO UPDATE
 SET unique_planets=EXCLUDED.unique_planets,held_planets=EXCLUDED.held_planets`, b, start, end)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO structs.influence_rollup_coverage VALUES($1,$2,$1)
 ON CONFLICT(build_id) DO UPDATE SET complete_before_height=EXCLUDED.complete_before_height,refreshed_revision=EXCLUDED.refreshed_revision`, b, end)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "build=%d complete_before_height=%d\n", b, end)
	return nil
}
