package events

import "errors"

// ErrSkipWithWarn is the sentinel a handler returns when it has decided
// (deterministically) that this event is malformed in a known, recoverable
// way and should be skipped without polluting the per-block transaction or
// rolling back any writes.
//
// Routing behaviour:
//   - The router releases the SAVEPOINT normally (the handler is expected
//     to have written nothing — if it did, those writes commit; the
//     contract is "skip cleanly, no side effects").
//   - A HandlerError row is still emitted, but with Severity == "warn"
//     so operators can distinguish recoverable noise (e.g. genesis
//     emitting a partial grid attributeId) from real handler failures
//     that need investigation or replay.
//
// Use this only for cases where SQL triggers would have silently accepted
// the row — i.e. we're choosing to log + skip rather than diverge by
// erroring. Genuine chain bugs (non-numeric where the schema says
// numeric, etc.) should still return a plain error so they reach
// severity='error' and the operator runbook.
var ErrSkipWithWarn = errors.New("skip with warn")
