package db

import (
	"context"
	"encoding/json"
)

// HandlerError captures everything we want to log when a per-event handler
// fails inside the per-block transaction.
type HandlerError struct {
	ChainID      string
	Height       int64
	TxIndex      *int
	MsgIndex     *int
	EventIndex   *int
	CompositeKey string
	Payload      json.RawMessage // already JSON-encoded; can be nil
	Error        string
	// Severity is "error" (default) or "warn". Set by the router based
	// on whether the handler returned a plain error or ErrSkipWithWarn.
	// Empty defaults to "error" in WriteHandlerError so older test code
	// that doesn't populate it still lands in the safer category.
	Severity string
	Stack    string
}

// WriteHandlerError appends one row. Caller is responsible for serialization
// (the Querier here can be the per-block tx OR the pool; if it's the tx and
// the tx rolls back, the error log entry is lost too — see the writer in
// internal/sync/block.go for how that's avoided by writing through the pool
// after the tx aborts).
func WriteHandlerError(ctx context.Context, q Querier, e HandlerError) error {
	var payload any
	if len(e.Payload) > 0 {
		payload = e.Payload
	}
	severity := e.Severity
	if severity == "" {
		severity = "error"
	}
	_, err := q.Exec(ctx, `
		INSERT INTO sync_state.handler_error_log
			(chain_id, height, tx_index, msg_index, event_index, composite_key, payload, error, stack, severity, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
	`, e.ChainID, e.Height, e.TxIndex, e.MsgIndex, e.EventIndex, e.CompositeKey, payload, e.Error, e.Stack, severity)
	return err
}

// ResolveErrorsInRange marks all unresolved error rows in [a..b] as resolved
// by the named caller. Used by the replay subcommand.
func ResolveErrorsInRange(ctx context.Context, q Querier, chainID string, a, b int64, resolvedBy string) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE sync_state.handler_error_log
		   SET resolved_at = NOW(), resolved_by = $4
		 WHERE chain_id = $1 AND height BETWEEN $2 AND $3 AND resolved_at IS NULL
	`, chainID, a, b, resolvedBy)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UnresolvedErrorSummary counts unresolved handler_error_log rows for a
// chain, split by severity. Used by runIngest to render the startup banner
// and by `sync-state verify` to surface the same number.
func UnresolvedErrorSummary(ctx context.Context, q Querier, chainID string) (errCount, warnCount int64, err error) {
	rows, err := q.Query(ctx, `
		SELECT COALESCE(severity, 'error'), COUNT(*)
		  FROM sync_state.handler_error_log
		 WHERE chain_id = $1 AND resolved_at IS NULL
		 GROUP BY 1
	`, chainID)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var sev string
		var n int64
		if err := rows.Scan(&sev, &n); err != nil {
			return 0, 0, err
		}
		switch sev {
		case "warn":
			warnCount = n
		default:
			errCount += n
		}
	}
	return errCount, warnCount, rows.Err()
}

// UnresolvedError is one row from sync_state.handler_error_log, hydrated
// for the reprocess-errors subcommand.
type UnresolvedError struct {
	ID           int64
	Height       int64
	TxIndex      *int
	MsgIndex     *int
	EventIndex   *int
	CompositeKey string
	Payload      json.RawMessage
	Severity     string
	Error        string
}

// ListUnresolvedErrors loads unresolved handler errors for a chain, with
// optional filters. Composite-key match is exact (empty = any). Heights
// outside [since, until] are skipped (zero in either bound means "no
// limit on that side"). limit <= 0 returns all matches.
func ListUnresolvedErrors(ctx context.Context, q Querier, chainID, compositeKey, severity string, since, until int64, limit int) ([]UnresolvedError, error) {
	const sqlStmt = `
		SELECT id, height, tx_index, msg_index, event_index,
		       composite_key, payload, COALESCE(severity, 'error'), error
		  FROM sync_state.handler_error_log
		 WHERE chain_id = $1
		   AND resolved_at IS NULL
		   AND ($2 = '' OR composite_key = $2)
		   AND ($3 = '' OR COALESCE(severity, 'error') = $3)
		   AND ($4 = 0 OR height >= $4)
		   AND ($5 = 0 OR height <= $5)
		 ORDER BY height ASC, id ASC
		 LIMIT NULLIF($6, 0)
	`
	var limitArg any = limit
	if limit <= 0 {
		limitArg = 0
	}
	rows, err := q.Query(ctx, sqlStmt, chainID, compositeKey, severity, since, until, limitArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnresolvedError
	for rows.Next() {
		var r UnresolvedError
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Height, &r.TxIndex, &r.MsgIndex, &r.EventIndex,
			&r.CompositeKey, &payload, &r.Severity, &r.Error); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			r.Payload = json.RawMessage(payload)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveErrorByID marks a single handler_error_log row resolved.
// Returns the number of rows affected (0 if already resolved / missing).
func ResolveErrorByID(ctx context.Context, q Querier, id int64, resolvedBy string) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE sync_state.handler_error_log
		   SET resolved_at = NOW(), resolved_by = $2
		 WHERE id = $1 AND resolved_at IS NULL
	`, id, resolvedBy)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
