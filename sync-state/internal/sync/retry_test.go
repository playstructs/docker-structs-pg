package sync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsTransientInfraErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ctx canceled", context.Canceled, false},
		{"ctx deadline", context.DeadlineExceeded, false},

		{"syscall ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"syscall ECONNRESET", syscall.ECONNRESET, true},
		{"syscall ENETUNREACH", syscall.ENETUNREACH, true},
		{"syscall ETIMEDOUT", syscall.ETIMEDOUT, true},
		{"syscall EPIPE", syscall.EPIPE, true},

		{"wrapped ECONNREFUSED", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"wrapped ECONNRESET in tx error", fmt.Errorf("begin tx h=1: %w", fmt.Errorf("conn: %w", syscall.ECONNRESET)), true},

		// Note: pgconn.ConnectError has an unexported `err` field, so we
		// can't synthesize one here. The real-PG-down scenario is covered
		// transitively by the syscall.ECONNREFUSED and string-fallback
		// cases below; pgconn.ConnectError ultimately wraps a syscall.Errno
		// or a string containing "connection refused".
		{"pg connection_failure 08006", &pgconn.PgError{Code: "08006", Message: "connection_failure"}, true},
		{"pg admin_shutdown 57P01", &pgconn.PgError{Code: "57P01", Message: "admin_shutdown"}, true},
		{"pg cannot_connect_now 57P03", &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}, true},

		{"pg syntax error 42601 (NOT transient)", &pgconn.PgError{Code: "42601", Message: "syntax error"}, false},
		{"pg undefined column 42703 (NOT transient)", &pgconn.PgError{Code: "42703", Message: "column does not exist"}, false},
		{"pg unique violation 23505 (NOT transient)", &pgconn.PgError{Code: "23505", Message: "duplicate key"}, false},

		{"net.OpError dial conn refused",
			&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			true},
		{"net.OpError with i/o timeout substring",
			&net.OpError{Op: "read", Net: "tcp", Err: errors.New("i/o timeout")},
			true},

		{"string fallback connection refused",
			errors.New("failed to connect: connection refused"),
			true},
		{"string fallback broken pipe",
			errors.New("write tcp: broken pipe"),
			true},
		{"string fallback unexpected EOF",
			errors.New("server closed: unexpected EOF"),
			true},

		{"app-level chain mismatch (NOT transient)",
			errors.New("chain id changed mid-run: was foo, now bar"),
			false},
		{"app-level reorg (NOT transient)",
			errors.New("reorg detected: cursor last_height=10 hash=AAA but node now returns hash=BBB"),
			false},
		{"app-level handler error (NOT transient)",
			errors.New("apply h=42: bank handler: unknown denom 'xyz'"),
			false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isTransientInfraErr(tc.err)
			if got != tc.want {
				t.Fatalf("isTransientInfraErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestTransientBackoff_Curve(t *testing.T) {
	var b transientBackoff
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		transientBackoffMax,
		transientBackoffMax,
		transientBackoffMax,
	}
	for i, w := range want {
		got := b.Next()
		if got != w {
			t.Fatalf("attempt %d: got %s, want %s", i+1, got, w)
		}
	}
	b.Reset()
	if got := b.Next(); got != transientBackoffMin {
		t.Fatalf("after Reset: got %s, want %s", got, transientBackoffMin)
	}
}
