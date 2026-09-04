package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/payload"
)

// PlanetAttributeInputs is the dependency bundle runIngest hands the sweep.
type PlanetAttributeInputs struct {
	Pool *pgxpool.Pool

	LCDBase    string
	PageLimit  int
	HTTPClient *http.Client
}

// PlanetAttributeReport summarises one SweepPlanetAttributes run.
type PlanetAttributeReport struct {
	Fetched  int
	Upserted int64 // rows inserted or whose val/attribute_type changed
	Zeroed   int64 // local rows cleared to 0 because the chain no longer holds them
	Skipped  int   // non-planet or unknown attrType ids
	Elapsed  time.Duration
}

// planetAttrLabels mirrors events.planetAttrLabels (attrType 0..15). Kept
// here so backfill does not import the events package.
var planetAttrLabels = [...]string{
	"planetaryShield",
	"repairNetworkQuantity",
	"defensiveCannonQuantity",
	"coordinatedGlobalShieldNetworkQuantity",
	"lowOrbitBallisticsInterceptorNetworkQuantity",
	"advancedLowOrbitBallisticsInterceptorNetworkQuantity",
	"lowOrbitBallisticsInterceptorNetworkSuccessRateNumerator",
	"lowOrbitBallisticsInterceptorNetworkSuccessRateDenominator",
	"orbitalJammingStationQuantity",
	"advancedOrbitalJammingStationQuantity",
	"blockStartRaid",
	"blockRaiderArrived",
	"blockStartOreMine",
	"blockStartOreRefine",
	"oreMiningActiveQuantity",
	"oreRefiningActiveQuantity",
}

// SweepPlanetAttributes pages the LCD planet-attribute store and reconciles
// structs.planet_attribute (all types, not only ore clocks). Forces
// attribute_type from the id prefix, upserts val, and sets val=0 on local
// planet rows the chain no longer holds (keep-zero). No planet_activity
// emits — clients re-read the store. Unreachable LCD degrades to a warning
// at the call site; this function just returns the error.
func SweepPlanetAttributes(ctx context.Context, in PlanetAttributeInputs) (PlanetAttributeReport, error) {
	started := time.Now()
	var r PlanetAttributeReport

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

	records, err := fetchAllPlanetAttributes(ctx, client, in.LCDBase, pageLimit)
	if err != nil {
		return r, fmt.Errorf("LCD fetch: %w", err)
	}
	r.Fetched = len(records)

	rows, skipped := planetAttributeRows(records)
	r.Skipped = skipped
	// Refuse an empty usable set: the zero-missing UPDATE would otherwise
	// clear every planet attribute row when the LCD is misconfigured or
	// returns an empty page.
	if len(rows) == 0 {
		return r, fmt.Errorf("LCD returned no usable planet attribute rows (fetched=%d skipped=%d); refusing to reconcile",
			r.Fetched, r.Skipped)
	}

	if err := applyPlanetAttributes(ctx, in.Pool, rows, &r); err != nil {
		return r, err
	}
	r.Elapsed = time.Since(started)
	return r, nil
}

type planetAttrRow struct {
	ID            string
	ObjectID      string
	AttributeType string
	Val           int64
}

// planetAttributeRows filters to planet objectTypeId=2 with a known label.
func planetAttributeRows(records []payload.PlanetAttribute) ([]planetAttrRow, int) {
	out := make([]planetAttrRow, 0, len(records))
	skipped := 0
	for _, rec := range records {
		id := strings.TrimSpace(rec.AttributeID)
		parts := strings.Split(id, "-")
		if len(parts) != 3 || parts[1] != "2" {
			skipped++
			continue
		}
		attrType, err := strconv.Atoi(parts[0])
		if err != nil || attrType < 0 || attrType >= len(planetAttrLabels) {
			skipped++
			continue
		}
		var val int64
		if rec.Value != "" {
			val, err = strconv.ParseInt(rec.Value, 10, 64)
			if err != nil {
				skipped++
				continue
			}
		}
		out = append(out, planetAttrRow{
			ID:            id,
			ObjectID:      parts[1] + "-" + parts[2],
			AttributeType: planetAttrLabels[attrType],
			Val:           val,
		})
	}
	return out, skipped
}

const createChainPATempSQL = `
CREATE TEMP TABLE chain_pa (
    id text PRIMARY KEY,
    object_id text NOT NULL,
    attribute_type text NOT NULL,
    val bigint NOT NULL
) ON COMMIT DROP`

const upsertPlanetAttributeFromChainSQL = `
INSERT INTO structs.planet_attribute (id, object_id, object_type, attribute_type, val, updated_at)
SELECT c.id, c.object_id, 'planet', c.attribute_type, c.val, NOW()
  FROM chain_pa c
ON CONFLICT (id) DO UPDATE
   SET val            = EXCLUDED.val,
       object_id      = EXCLUDED.object_id,
       object_type    = EXCLUDED.object_type,
       attribute_type = EXCLUDED.attribute_type,
       updated_at     = EXCLUDED.updated_at
 WHERE structs.planet_attribute.val            IS DISTINCT FROM EXCLUDED.val
    OR structs.planet_attribute.attribute_type IS DISTINCT FROM EXCLUDED.attribute_type
    OR structs.planet_attribute.object_id      IS DISTINCT FROM EXCLUDED.object_id
    OR structs.planet_attribute.object_type    IS DISTINCT FROM EXCLUDED.object_type`

// Rows the chain pruned (cleared to zero) stay at val=0 for keep-zero /
// updated_since. Only planet-scoped ids (object_type segment = 2).
const zeroMissingPlanetAttributeSQL = `
UPDATE structs.planet_attribute p
   SET val = 0,
       updated_at = NOW()
 WHERE split_part(p.id, '-', 2) = '2'
   AND p.val IS DISTINCT FROM 0
   AND NOT EXISTS (SELECT 1 FROM chain_pa c WHERE c.id = p.id)`

func applyPlanetAttributes(ctx context.Context, pool *pgxpool.Pool, rows []planetAttrRow, r *PlanetAttributeReport) error {
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

	if _, err := tx.Exec(ctx, createChainPATempSQL); err != nil {
		return fmt.Errorf("create temp chain_pa: %w", err)
	}

	if len(rows) > 0 {
		copySrc := make([][]any, len(rows))
		for i, row := range rows {
			copySrc[i] = []any{row.ID, row.ObjectID, row.AttributeType, row.Val}
		}
		_, err := tx.CopyFrom(ctx,
			pgx.Identifier{"chain_pa"},
			[]string{"id", "object_id", "attribute_type", "val"},
			pgx.CopyFromRows(copySrc),
		)
		if err != nil {
			return fmt.Errorf("copy chain_pa: %w", err)
		}
	}

	tag, err := tx.Exec(ctx, upsertPlanetAttributeFromChainSQL)
	if err != nil {
		return fmt.Errorf("upsert planet_attribute: %w", err)
	}
	r.Upserted = tag.RowsAffected()

	tag, err = tx.Exec(ctx, zeroMissingPlanetAttributeSQL)
	if err != nil {
		return fmt.Errorf("zero missing planet_attribute: %w", err)
	}
	r.Zeroed = tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// PrintPlanetAttributeReport writes the one-or-two line startup summary.
func PrintPlanetAttributeReport(w io.Writer, r PlanetAttributeReport) {
	if r.Upserted == 0 && r.Zeroed == 0 {
		fmt.Fprintf(w, "Planet attribute sweep: in sync (%d record(s) checked in %s)\n",
			r.Fetched, r.Elapsed.Round(time.Millisecond))
	} else {
		fmt.Fprintf(w, "Planet attribute sweep: upserted %d, zeroed %d of %d LCD record(s) in %s\n",
			r.Upserted, r.Zeroed, r.Fetched, r.Elapsed.Round(time.Millisecond))
	}
	if r.Skipped > 0 {
		fmt.Fprintf(w, "  skipped %d non-planet or unknown-type record(s)\n", r.Skipped)
	}
}

// fetchAllPlanetAttributes pages GET {lcd}/structs/structs/planet_attribute
// until pagination.next_key is empty.
func fetchAllPlanetAttributes(ctx context.Context, client *http.Client, lcdBase string, pageLimit int) ([]payload.PlanetAttribute, error) {
	var all []payload.PlanetAttribute
	var nextKey string
	for {
		u := fmt.Sprintf("%s/structs/structs/planet_attribute?pagination.limit=%d",
			trimTrailingSlash(lcdBase), pageLimit)
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

		var page planetAttributePage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		if len(page.PlanetAttributeRecords) == 0 {
			break
		}
		all = append(all, page.PlanetAttributeRecords...)

		if page.Pagination.NextKey == nil || *page.Pagination.NextKey == "" {
			break
		}
		nextKey = *page.Pagination.NextKey
	}
	return all, nil
}

// planetAttributePage matches the LCD QueryAllPlanetAttributeResponse JSON.
type planetAttributePage struct {
	PlanetAttributeRecords []payload.PlanetAttribute `json:"planetAttributeRecords"`
	Pagination             struct {
		NextKey *string `json:"next_key"`
	} `json:"pagination"`
}
