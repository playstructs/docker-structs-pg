package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UnknownDelta is the per-composite_key accumulator handed to
// UpsertUnknownEntries by the syncer. Mirrors events.UnknownDelta but
// kept independent so the db package doesn't import events (avoids a
// circular dependency).
type UnknownDelta struct {
	Count         uint64
	FirstHeight   int64
	LastHeight    int64
	SamplePayload json.RawMessage
}

// UpsertUnknownEntries flushes a router snapshot into
// sync_state.unknown_event_log. Counts accumulate (DB count += delta
// count), heights expand (LEAST/GREATEST), and the sample payload is
// overwritten with the most recent value so operators always see fresh
// data.
//
// Empty input is a no-op. Errors propagate; the caller logs and
// continues (a failed flush only delays operator visibility, never
// blocks ingest).
func UpsertUnknownEntries(ctx context.Context, pool *pgxpool.Pool, chainID string, entries map[string]UnknownDelta) error {
	if len(entries) == 0 {
		return nil
	}

	// Single multi-row INSERT keeps round-trip count to one regardless
	// of how many distinct unknowns landed in the window.
	const baseCols = 6 // chain_id, composite_key, count, first, last, payload
	rows := len(entries)
	args := make([]any, 0, rows*baseCols)
	values := make([]string, 0, rows)
	i := 0
	for key, d := range entries {
		off := i * baseCols
		values = append(values, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d::jsonb)",
			off+1, off+2, off+3, off+4, off+5, off+6,
		))
		var payload any
		if len(d.SamplePayload) > 0 {
			payload = string(d.SamplePayload)
		}
		args = append(args,
			chainID,
			key,
			int64(d.Count),
			d.FirstHeight,
			d.LastHeight,
			payload,
		)
		i++
	}

	sql := `INSERT INTO sync_state.unknown_event_log
		(chain_id, composite_key, count, first_seen_height, last_seen_height, last_payload)
		VALUES ` + strings.Join(values, ", ") + `
		ON CONFLICT (chain_id, composite_key) DO UPDATE
		SET count             = sync_state.unknown_event_log.count + EXCLUDED.count,
		    first_seen_height = LEAST(sync_state.unknown_event_log.first_seen_height, EXCLUDED.first_seen_height),
		    last_seen_height  = GREATEST(sync_state.unknown_event_log.last_seen_height, EXCLUDED.last_seen_height),
		    last_seen_at      = NOW(),
		    last_payload      = COALESCE(EXCLUDED.last_payload, sync_state.unknown_event_log.last_payload)`

	_, err := pool.Exec(ctx, sql, args...)
	return err
}
