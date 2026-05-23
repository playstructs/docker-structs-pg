package sync

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// isTransientInfraErr reports whether err looks like a recoverable
// infrastructure failure that has a reasonable chance of self-resolving
// (Postgres restart, RPC node bounce, ephemeral network blip) — as
// opposed to a logic/data failure that won't get better by waiting
// (chain hash mismatch, schema drift, payload corruption).
//
// The classifier is intentionally generous on the transient side:
// false-negatives leave the loop crashing on every PG / RPC bounce, which
// is the bug we're fixing here. False-positives just stall the loop for a
// few exponential-backoff cycles before bubbling — operators still see
// the error in logs every retry, so a "stuck retrying a real bug" failure
// mode is loud, not silent.
//
// What counts as transient
//   - context.{Canceled,DeadlineExceeded} is NOT transient: shutdown is
//     in progress, caller wants to exit, retrying would loop forever.
//   - syscall.ECONNREFUSED / ECONNRESET / ENETUNREACH / EHOSTUNREACH /
//     ETIMEDOUT / EPIPE: classic dial / mid-connection failure modes.
//   - net.Error with Timeout(): I/O deadline hit (covers both fetch and
//     apply paths since both use ctx-bound network calls).
//   - pgconn.ConnectError: pgx's wrapper for dial-time failure.
//   - pgconn.PgError SQLSTATEs in class 08 (connection_exception) or 57
//     (operator_intervention — admin_shutdown, crash_shutdown,
//     cannot_connect_now). Either side dropped the conn server-side.
//   - String fallback for "connection refused" / "broken pipe" / "EOF" /
//     "no route to host" / "i/o timeout". pgx and pgxpool sometimes
//     deliver these as plain *fmt.wrapError wrapping the lower-level
//     error, and errors.As doesn't always reach through. Belt-and-braces.
//
// What is NOT transient (selected examples)
//   - Application errors from handler logic (will reappear every retry).
//   - SQL syntax errors / column-does-not-exist (schema drift; retry
//     won't help).
//   - The chain-id-mismatch error from Run (this is a hard divergence,
//     not a flaky link).
func isTransientInfraErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var sysErr syscall.Errno
	if errors.As(err, &sysErr) {
		switch sysErr {
		case syscall.ECONNREFUSED,
			syscall.ECONNRESET,
			syscall.ENETUNREACH,
			syscall.EHOSTUNREACH,
			syscall.ETIMEDOUT,
			syscall.EPIPE:
			return true
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08 — connection_exception (08000, 08003, 08006, 08001, 08004, 08007, 08P01).
		// Class 57 — operator_intervention (57P01 admin_shutdown,
		//            57P02 crash_shutdown, 57P03 cannot_connect_now,
		//            57P04 database_dropped).
		if strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57") {
			return true
		}
	}

	// String fallback — covers the cases above when wrapped layers strip
	// the typed error. Cheap to evaluate and only runs after typed
	// classification fails.
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return true
	case strings.Contains(s, "connection reset"):
		return true
	case strings.Contains(s, "broken pipe"):
		return true
	case strings.Contains(s, "no route to host"):
		return true
	case strings.Contains(s, "i/o timeout"):
		return true
	case strings.Contains(s, "unexpected EOF"):
		return true
	case strings.Contains(s, "EOF"):
		// Bare "EOF" is what pgx sometimes returns when a server-side
		// shutdown happens mid-query. False-positive risk is low — a
		// genuine EOF from application code wouldn't bubble through
		// fetch/apply.
		return true
	}
	return false
}

// transientBackoff is the per-Run retry-delay state for transient infra
// errors. Reset() after every successful fetch+apply cycle so a single
// recovered blip doesn't permanently scale the next backoff.
//
// Delays: 1s, 2s, 4s, 8s, 16s, 30s, 30s, 30s, ...
// Total wall-clock to first 30s cap: ~63s.
type transientBackoff struct {
	delay time.Duration
}

const transientBackoffMin = time.Second
const transientBackoffMax = 30 * time.Second

func (b *transientBackoff) Next() time.Duration {
	if b.delay <= 0 {
		b.delay = transientBackoffMin
		return b.delay
	}
	b.delay *= 2
	if b.delay > transientBackoffMax {
		b.delay = transientBackoffMax
	}
	return b.delay
}

func (b *transientBackoff) Reset() {
	b.delay = 0
}
