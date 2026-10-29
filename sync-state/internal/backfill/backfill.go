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
//   - SweepPlanetAttributes: EventPlanetAttribute only rewrites when val
//     changes, so a missed block range (e.g. ore-clock cutover) leaves
//     stale rows until the next chain change. Re-seeds all planet
//     attribute types from the LCD store and zeros pruned ids.
//   - SweepStaleDefenders: the chain never emits EventStructDefenderClear
//     when the *protected* struct dies. Deletes orphaned
//     struct_defender + protectedStructIndex attribute rows. Pure SQL;
//     no chain access required.
//   - SweepDefenderPlanetary: reconciles struct_defender.is_planetary
//     against the protected struct's location_type. Clears drift from
//     rows written before the handler learned to set the column.
package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/lcd"
	"sync-state/internal/payload"
	"sync-state/internal/readmodel"
)

// PlayerRankInputs is the dependency bundle runIngest hands the sweep.
// Mirrors fields on sync.Config so backfill stays free of an import
// cycle with the sync package.
type PlayerRankInputs struct {
	Pool    *pgxpool.Pool
	ChainID string

	// LCDBase is the Cosmos LCD base URL (e.g. http://structsd:1317).
	LCDBase string
	// PageLimit is the pagination.limit sent to the LCD. 0 → 10000.
	PageLimit int

	// HTTPClient is optional; when nil a 60s-timeout client is used.
	HTTPClient *http.Client
}

// PlayerRankReport summarises one sweep.
type PlayerRankReport struct {
	Height         int64
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

	boundary, err := readBoundary(ctx, in.Pool, in.ChainID)
	if err != nil {
		return r, err
	}
	r.Height = boundary.height
	if boundary.height == 0 {
		return r, nil // No committed indexed snapshot exists yet.
	}
	gateway := lcd.Client{BaseURL: in.LCDBase, HTTP: client}
	if err := gateway.ValidateChain(ctx, in.ChainID); err != nil {
		return r, err
	}
	players, err := fetchAllPlayers(ctx, client, in.LCDBase, pageLimit, boundary.height)
	if err != nil {
		return r, fmt.Errorf("LCD fetch: %w", err)
	}
	r.Fetched = len(players)
	if len(players) == 0 {
		r.Elapsed = time.Since(started)
		return r, nil
	}

	if err := applyGuildRanks(ctx, in.Pool, players, &r, boundary); err != nil {
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
           unnest($2::text[])::numeric AS rank
  ) v
 WHERE p.id = v.id
   AND p.guild_rank IS DISTINCT FROM v.rank`

const countPresentSQL = `SELECT COUNT(*) FROM structs.player WHERE id = ANY($1::text[])`

// applyGuildRanks runs the batched UPDATE in a single transaction.
func applyGuildRanks(ctx context.Context, pool *pgxpool.Pool, players []payload.Player, r *PlayerRankReport, boundary snapshotBoundary) error {
	ids := make([]string, len(players))
	ranks := make([]string, len(players))
	for i, p := range players {
		ids[i] = p.ID
		ranks[i] = p.GuildRank.String()
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

	if err := readmodel.Lock(ctx, tx); err != nil {
		return err
	}

	if err := boundary.check(ctx, tx); err != nil {
		return err
	}

	// A complete snapshot at this same height must resolve every player.
	var present int64
	if err := tx.QueryRow(ctx, countPresentSQL, ids).Scan(&present); err != nil {
		return fmt.Errorf("count present: %w", err)
	}
	r.MissingInDB = len(players) - int(present)
	if r.MissingInDB != 0 {
		return fmt.Errorf("same-height snapshot contains %d players absent from indexed state", r.MissingInDB)
	}

	tag, err := tx.Exec(ctx, updateGuildRankSQL, ids, ranks)
	if err != nil {
		return fmt.Errorf("update guild_rank: %w", err)
	}
	r.Updated = tag.RowsAffected()
	r.AlreadyCorrect = present - r.Updated
	if r.AlreadyCorrect < 0 {
		r.AlreadyCorrect = 0
	}

	if r.Updated > 0 {
		if err := readmodel.Invalidate(ctx, tx); err != nil {
			return err
		}
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

	if err := readmodel.Lock(ctx, tx); err != nil {
		return r, err
	}

	if err := tx.QueryRow(ctx, sweepStaleDefendersSQL).Scan(&r.DefendersRemoved, &r.AttrsRemoved); err != nil {
		return r, fmt.Errorf("sweep stale defenders: %w", err)
	}
	if r.DefendersRemoved+r.AttrsRemoved > 0 {
		if err := readmodel.Invalidate(ctx, tx); err != nil {
			return r, err
		}
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

// DefenderPlanetaryReport summarises one SweepDefenderPlanetary run.
type DefenderPlanetaryReport struct {
	Updated int64
	Elapsed time.Duration
}

// sweepDefenderPlanetaryMatchedSQL sets is_planetary from the protected
// struct's location_type. Does not touch updated_at — matching the
// structs-pg backfill — so that column keeps meaning "when the chain
// last changed this relationship".
const sweepDefenderPlanetaryMatchedSQL = `
UPDATE structs.struct_defender d
   SET is_planetary = (s.location_type = 'planet')
  FROM structs.struct s
 WHERE s.id = d.protected_struct_id
   AND d.is_planetary IS DISTINCT FROM (s.location_type = 'planet')`

// sweepDefenderPlanetaryOrphanSQL clears is_planetary on rows whose
// protected struct is missing (migration EXISTS semantics → false).
const sweepDefenderPlanetaryOrphanSQL = `
UPDATE structs.struct_defender d
   SET is_planetary = FALSE
 WHERE d.is_planetary
   AND NOT EXISTS (SELECT 1 FROM structs.struct s WHERE s.id = d.protected_struct_id)`

// SweepDefenderPlanetary reconciles structs.struct_defender.is_planetary
// against the protected struct's location_type. Pure SQL, no chain
// access. No-ops once every row agrees with the intended state. Does
// not touch updated_at.
func SweepDefenderPlanetary(ctx context.Context, pool *pgxpool.Pool) (DefenderPlanetaryReport, error) {
	started := time.Now()
	var r DefenderPlanetaryReport

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

	if err := readmodel.Lock(ctx, tx); err != nil {
		return r, err
	}

	updated, err := applyDefenderPlanetary(ctx, tx)
	if err != nil {
		return r, err
	}
	r.Updated = updated
	if r.Updated > 0 {
		if err := readmodel.Invalidate(ctx, tx); err != nil {
			return r, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return r, fmt.Errorf("commit: %w", err)
	}
	committed = true
	r.Elapsed = time.Since(started)
	return r, nil
}

// applyDefenderPlanetary runs both reconciliation UPDATEs on an open
// transaction. Used by SweepDefenderPlanetary and by the package's
// integration test (rolled-back dry run).
func applyDefenderPlanetary(ctx context.Context, tx pgx.Tx) (int64, error) {
	tag, err := tx.Exec(ctx, sweepDefenderPlanetaryMatchedSQL)
	if err != nil {
		return 0, fmt.Errorf("sweep defender is_planetary (matched): %w", err)
	}
	updated := tag.RowsAffected()
	tag, err = tx.Exec(ctx, sweepDefenderPlanetaryOrphanSQL)
	if err != nil {
		return 0, fmt.Errorf("sweep defender is_planetary (orphan): %w", err)
	}
	return updated + tag.RowsAffected(), nil
}

// PrintDefenderPlanetaryReport writes the one-line startup summary.
func PrintDefenderPlanetaryReport(w io.Writer, r DefenderPlanetaryReport) {
	if r.Updated == 0 {
		fmt.Fprintf(w, "Defender is_planetary sweep: in sync (%s)\n", r.Elapsed.Round(time.Millisecond))
		return
	}
	fmt.Fprintf(w, "Defender is_planetary sweep: repaired %d row(s) in %s\n",
		r.Updated, r.Elapsed.Round(time.Millisecond))
}

// fetchAllPlayers pages GET {lcd}/structs/player until pagination.next_key
// is empty. Same loop shape as update-cache's fetchAllPages.
func fetchAllPlayers(ctx context.Context, client *http.Client, lcdBase string, pageLimit int, height int64) ([]payload.Player, error) {
	var all []payload.Player
	seen := map[string]bool{}
	gateway := lcd.Client{BaseURL: lcdBase, HTTP: client}
	err := gateway.Pages(ctx, "/structs/player", "Player", height, pageLimit, func(raw json.RawMessage) error {
		p, err := payload.DecodeRequired[struct {
			ID string `json:"id"`
			GuildRank payload.Uint64 `json:"guildRank"`
		}](raw, "id", "guildRank")
		if err != nil {
			return err
		}
		if !validObjectID(p.ID, "1") || seen[p.ID] {
			return fmt.Errorf("invalid or duplicate player ID %q", p.ID)
		}
		seen[p.ID] = true
		all = append(all, payload.Player{ID: p.ID, GuildRank: p.GuildRank})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}
