// Package backfill repairs structs.* state that historical event
// ingestion missed (or that the chain itself is lazy about emitting).
//
// Sweeps run from runIngest at startup, before the syncer loop begins.
// That placement is load-bearing: ingest already owns the writer lock
// and no handler is running yet, so a full refresh cannot race a
// handler write. Each sweep is cheap enough to run unconditionally —
// once the table agrees with the intended state the statements write
// zero rows.
//
// Current sweeps:
//
//   - SweepPlayerRanks: EventPlayer historically omitted guildRank (a
//     20260427 SQL rewrite bug). Reconciles against the LCD snapshot.
//   - SweepStaleDefenders: the chain never emits EventStructDefenderClear
//     when the *protected* struct dies. Deletes orphaned
//     struct_defender + protectedStructIndex attribute rows. Pure SQL;
//     no chain access required.
package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/payload"
)

// PlayerRankInputs is the dependency bundle runIngest hands the sweep.
// Mirrors fields on sync.Config so backfill stays free of an import
// cycle with the sync package.
type PlayerRankInputs struct {
	Pool *pgxpool.Pool

	// LCDBase is the Cosmos LCD base URL (e.g. http://structsd:1317).
	LCDBase string
	// PageLimit is the pagination.limit sent to the LCD. 0 → 10000.
	PageLimit int

	// HTTPClient is optional; when nil a 60s-timeout client is used.
	HTTPClient *http.Client
}

// PlayerRankReport summarises one sweep.
type PlayerRankReport struct {
	Fetched        int
	Updated        int64
	AlreadyCorrect int64
	MissingInDB    int
	Elapsed        time.Duration
}

// SweepPlayerRanks pages the LCD player snapshot and reconciles
// structs.player.guild_rank. Returns the report so the caller can decide
// how loudly to log; errors are returned rather than fatal so a
// unreachable LCD degrades to a warning instead of blocking ingest.
func SweepPlayerRanks(ctx context.Context, in PlayerRankInputs) (PlayerRankReport, error) {
	started := time.Now()
	var r PlayerRankReport

	if in.LCDBase == "" {
		return r, fmt.Errorf("no LCD base URL configured (-lcd / STRUCTS_API_URL)")
	}
	pageLimit := in.PageLimit
	if pageLimit <= 0 {
		pageLimit = 10000
	}
	client := in.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	players, err := fetchAllPlayers(ctx, client, in.LCDBase, pageLimit)
	if err != nil {
		return r, fmt.Errorf("LCD fetch: %w", err)
	}
	r.Fetched = len(players)
	if len(players) == 0 {
		r.Elapsed = time.Since(started)
		return r, nil
	}

	if err := applyGuildRanks(ctx, in.Pool, players, &r); err != nil {
		return r, err
	}
	r.Elapsed = time.Since(started)
	return r, nil
}

const updateGuildRankSQL = `
UPDATE structs.player p
   SET guild_rank = v.rank,
       updated_at = NOW()
  FROM (
    SELECT unnest($1::text[])   AS id,
           unnest($2::bigint[]) AS rank
  ) v
 WHERE p.id = v.id
   AND p.guild_rank IS DISTINCT FROM v.rank`

const countPresentSQL = `SELECT COUNT(*) FROM structs.player WHERE id = ANY($1::text[])`

// applyGuildRanks runs the batched UPDATE in a single transaction.
func applyGuildRanks(ctx context.Context, pool *pgxpool.Pool, players []payload.Player, r *PlayerRankReport) error {
	ids := make([]string, len(players))
	ranks := make([]int64, len(players))
	for i, p := range players {
		ids[i] = p.ID
		ranks[i] = p.GuildRank.Int64()
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Chain ids the DB has never seen mean ingest is behind the LCD
	// snapshot; worth surfacing but not an error.
	var present int64
	if err := tx.QueryRow(ctx, countPresentSQL, ids).Scan(&present); err != nil {
		return fmt.Errorf("count present: %w", err)
	}
	r.MissingInDB = len(players) - int(present)

	tag, err := tx.Exec(ctx, updateGuildRankSQL, ids, ranks)
	if err != nil {
		return fmt.Errorf("update guild_rank: %w", err)
	}
	r.Updated = tag.RowsAffected()
	r.AlreadyCorrect = present - r.Updated
	if r.AlreadyCorrect < 0 {
		r.AlreadyCorrect = 0
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// PrintPlayerRankReport writes the one-or-two line startup summary.
func PrintPlayerRankReport(w io.Writer, r PlayerRankReport) {
	if r.Updated == 0 {
		fmt.Fprintf(w, "Player guild_rank sweep: in sync (%d player(s) checked in %s)\n",
			r.Fetched, r.Elapsed.Round(time.Millisecond))
	} else {
		fmt.Fprintf(w, "Player guild_rank sweep: repaired %d of %d player(s) in %s\n",
			r.Updated, r.Fetched, r.Elapsed.Round(time.Millisecond))
	}
	if r.MissingInDB > 0 {
		fmt.Fprintf(w, "  %d chain player(s) not yet in structs.player (ingest is behind the snapshot)\n",
			r.MissingInDB)
	}
}

// StaleDefenderReport summarises one SweepStaleDefenders run.
type StaleDefenderReport struct {
	DefendersRemoved int64
	AttrsRemoved     int64
	Elapsed          time.Duration
}

// sweepStaleDefendersSQL deletes every struct_defender row whose
// protected struct is already destroyed, then drops the paired
// protectedStructIndex attribute rows (id = '5-' || defending_struct_id,
// matching structDefenderClearAttributeSQL). No planet_activity emits —
// back-dating into the seq-ordered hypertable would risk verify's
// monotonicity checks; going-forward cleanups emit via the handler.
const sweepStaleDefendersSQL = `
WITH stale AS (
    DELETE FROM structs.struct_defender d
     USING structs.struct s
     WHERE s.id = d.protected_struct_id
       AND s.is_destroyed
    RETURNING d.defending_struct_id
),
attrs AS (
    DELETE FROM structs.struct_attribute
     WHERE id IN (SELECT '5-' || defending_struct_id FROM stale)
    RETURNING id
)
SELECT
    (SELECT count(*) FROM stale)::bigint AS defenders_removed,
    (SELECT count(*) FROM attrs)::bigint AS attrs_removed`

// SweepStaleDefenders removes orphaned defender relationships left behind
// when a protected struct was destroyed without a matching
// EventStructDefenderClear. Pure SQL, no chain access — an unreachable
// LCD cannot block this sweep. No-ops once the table is clean.
func SweepStaleDefenders(ctx context.Context, pool *pgxpool.Pool) (StaleDefenderReport, error) {
	started := time.Now()
	var r StaleDefenderReport

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return r, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := tx.QueryRow(ctx, sweepStaleDefendersSQL).Scan(&r.DefendersRemoved, &r.AttrsRemoved); err != nil {
		return r, fmt.Errorf("sweep stale defenders: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return r, fmt.Errorf("commit: %w", err)
	}
	committed = true
	r.Elapsed = time.Since(started)
	return r, nil
}

// PrintStaleDefenderReport writes the one-line startup summary.
func PrintStaleDefenderReport(w io.Writer, r StaleDefenderReport) {
	if r.DefendersRemoved == 0 {
		fmt.Fprintf(w, "Stale defender sweep: in sync (%s)\n", r.Elapsed.Round(time.Millisecond))
		return
	}
	fmt.Fprintf(w, "Stale defender sweep: removed %d defender row(s) and %d attribute row(s) in %s\n",
		r.DefendersRemoved, r.AttrsRemoved, r.Elapsed.Round(time.Millisecond))
}

// fetchAllPlayers pages GET {lcd}/structs/player until pagination.next_key
// is empty. Same loop shape as update-cache's fetchAllPages.
func fetchAllPlayers(ctx context.Context, client *http.Client, lcdBase string, pageLimit int) ([]payload.Player, error) {
	var all []payload.Player
	var nextKey string
	for {
		u := fmt.Sprintf("%s/structs/player?pagination.limit=%d", trimTrailingSlash(lcdBase), pageLimit)
		if nextKey != "" {
			u += "&pagination.key=" + url.QueryEscape(nextKey)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, u, truncate(string(body), 200))
		}

		var page playerPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		if len(page.Player) == 0 {
			break
		}
		all = append(all, page.Player...)

		if page.Pagination.NextKey == nil || *page.Pagination.NextKey == "" {
			break
		}
		nextKey = *page.Pagination.NextKey
	}
	return all, nil
}

// playerPage matches the LCD QueryAllPlayerResponse JSON shape.
type playerPage struct {
	Player     []payload.Player `json:"Player"`
	Pagination struct {
		NextKey *string `json:"next_key"`
	} `json:"pagination"`
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
