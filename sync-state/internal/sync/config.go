// Package sync wires the rpc client, db pool, doctor, events router, and
// bank buffer together into the actual block-ingestion loop. Subcommands
// (ingest, bootstrap, doctor, list-handlers) all enter through this package.
package sync

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultRPCSeed is the always-on public RPC fallback. Hard-coded so
// `sync-state` runs against the testnet with no env vars, and so the
// docker-structs-guild deployment automatically gets a viable bootstrap
// path even before the local structsd is reachable.
const DefaultRPCSeed = "https://public.testnet.structs.network:26657"

// Config captures every knob the binary exposes via flags + env vars.
// Defaults are chosen for a private structsd:26657 + local Postgres.
type Config struct {
	// RPC + DB

	// RPCSeed is the always-on fallback endpoint. Defaults to
	// DefaultRPCSeed. Must always be a reachable RPC.
	RPCSeed string

	// RPCPrimary is the operator-preferred endpoint (e.g. a local
	// structsd). Empty means "no primary, use seed only". When set, it
	// becomes index-0 of the RPC pool and the seed slides to index-1.
	RPCPrimary string

	// RPCDeprecationNotice carries a one-line warning when the operator
	// is still using the deprecated -rpc / STRUCTS_RPC_URL knob, so the
	// subcommand can surface it once at startup. Empty in the new path.
	RPCDeprecationNotice string

	DatabaseURL string
	DBMaxConns  int

	// Ingest knobs
	StartHeight    int64 // 0 = resume from sync_cursor; clamped to node's earliest
	StopHeight     int64 // 0 = follow tip forever
	BatchSize      int
	Parallelism    int
	PollInterval   time.Duration
	HTTPTimeout    time.Duration
	HTTPMaxRetries int
	LogEvery       int
	OneShot        bool

	// StatementTimeout is applied via `SET LOCAL statement_timeout`
	// at the start of every per-block transaction. Any single SQL
	// statement that runs longer than this gets cancelled by Postgres
	// with SQLSTATE 57014 ("query_canceled"); the per-handler savepoint
	// then catches the error and logs it to handler_error_log so the
	// block as a whole still commits and the syncer keeps moving.
	//
	// 0 = no timeout (legacy behaviour; a hung statement stalls the
	// entire syncer indefinitely). Default 60s — large enough that
	// no healthy block-write should ever hit it, small enough that an
	// unhealthy one becomes a logged error within a minute instead of
	// silently freezing ingestion.
	StatementTimeout time.Duration

	// Behaviour
	MirrorRaw           bool
	StrictUnknownEvents bool
	SkipDoctor          bool

	// TipWebsocket enables the CometBFT `tm.event='NewBlock'` push
	// subscription so the tip-idle loop wakes the moment a block is
	// committed instead of waiting for the next /status poll.
	// Default true. The poll interval becomes a failsafe (covers
	// dropped websockets, dial gaps, and nodes that don't expose
	// /websocket) rather than the primary signal.
	TipWebsocket bool

	// `sync-state verify` knobs. Surfaced on Config so the top-level
	// flag parser owns them; verify reads them directly.
	VerifyErrorsOnly  bool
	VerifyWriteReport bool
	VerifyJSON        bool
	VerifyLagWarn     int64

	// `sync-state reprocess-errors` knobs. Same rationale as verify.
	ReprocessCompositeKey string
	ReprocessSeverity     string
	ReprocessSince        int64
	ReprocessUntil        int64
	ReprocessLimit        int
	ReprocessDryRun       bool

	// `sync-state init-genesis` knobs.
	//
	// GenesisFile overrides the default RPC fetch; empty = use RPC.
	// ForceGenesis allows re-applying when genesis_log already records a
	// successful apply for this chain. GenesisAutoApply controls whether
	// runIngest auto-applies genesis on first start from height 1 — true
	// is the default convenience knob; set false to require an explicit
	// `sync-state init-genesis` step (matches the historical
	// docker-structsd workflow for operators who want hand-control).
	GenesisFile      string
	ForceGenesis     bool
	GenesisAutoApply bool

	// Bulk-load mode (deferred commit). When lag >= BulkLagThreshold,
	// applyWindow runs blocks [start..end] in one outer PG transaction.
	// Event order and per-row writes are unchanged; only commit frequency
	// and cursor/heartbeat cadence differ. Disabled at tip (lag < threshold).
	BulkEnabled            bool
	BulkWindow             int
	BulkLagThreshold       int
	BulkStatementTimeout   time.Duration
}

// LoadConfig parses CLI flags and env vars. Flag defaults are pulled from env
// so the same binary works either way.
func LoadConfig(args []string) Config {
	var cfg Config
	fs := flag.NewFlagSet("sync-state", flag.ContinueOnError)

	// Two-tier RPC config. Operators set primary (e.g. local structsd);
	// seed stays at the public default unless explicitly overridden.
	fs.StringVar(&cfg.RPCSeed, "rpc-seed", envOr("STRUCTS_RPC_SEED", DefaultRPCSeed),
		"Always-on fallback CometBFT RPC URL (public node)")
	fs.StringVar(&cfg.RPCPrimary, "rpc-primary", envOr("STRUCTS_RPC_PRIMARY", ""),
		"Operator-preferred CometBFT RPC URL (e.g. http://structsd:26657); empty = use seed only")

	// Deprecated alias for -rpc-primary. Kept so existing scripts and
	// the docker-structs-pg compose unit keep working; we just emit a
	// deprecation notice and treat -rpc as a primary override.
	var legacyRPC string
	fs.StringVar(&legacyRPC, "rpc", envOr("STRUCTS_RPC_URL", ""),
		"DEPRECATED: alias for -rpc-primary; will be removed in a future release")

	fs.StringVar(&cfg.DatabaseURL, "db", "", "Postgres connection URL (else built from PG* env)")
	fs.IntVar(&cfg.DBMaxConns, "db-max-conns", envOrInt("SYNC_STATE_DB_MAX_CONNS", 4),
		"Max pgxpool connections (default 4; budget alongside update-cache + webapp)")

	fs.Int64Var(&cfg.StartHeight, "start", envOrInt64("SYNC_START_HEIGHT", 0),
		"Start height (0 = resume from sync_state.sync_cursor or node's earliest)")
	fs.Int64Var(&cfg.StopHeight, "stop", envOrInt64("SYNC_STOP_HEIGHT", 0),
		"Stop after this height (0 = follow tip)")
	fs.IntVar(&cfg.BatchSize, "batch", envOrInt("SYNC_BATCH_SIZE", 200),
		"Max blocks fetched per window")
	fs.IntVar(&cfg.Parallelism, "parallelism", envOrInt("SYNC_PARALLELISM", 8),
		"Concurrent RPC fetches")
	fs.DurationVar(&cfg.PollInterval, "poll", envOrDuration("SYNC_POLL_INTERVAL", 1*time.Second),
		"Failsafe sleep between tip-polls when caught up (primary wake signal is "+
			"the NewBlock WebSocket push when -tip-ws is enabled; the poll is a "+
			"belt-and-braces fallback that covers dropped sockets and dial gaps)")
	fs.DurationVar(&cfg.HTTPTimeout, "http-timeout", envOrDuration("SYNC_HTTP_TIMEOUT", 30*time.Second),
		"Per-request HTTP timeout")
	fs.IntVar(&cfg.HTTPMaxRetries, "http-retries", envOrInt("SYNC_HTTP_RETRIES", 5),
		"Per-request retry budget (exponential backoff)")
	fs.IntVar(&cfg.LogEvery, "log-every", envOrInt("SYNC_LOG_EVERY", 100),
		"Log progress every N blocks")
	fs.BoolVar(&cfg.OneShot, "one-shot", envOrBool("SYNC_ONE_SHOT", false),
		"Exit when caught up to current tip instead of following")
	fs.DurationVar(&cfg.StatementTimeout, "statement-timeout", envOrDuration("SYNC_STATE_STATEMENT_TIMEOUT", 60*time.Second),
		"Per-statement timeout applied to every per-block tx via SET LOCAL "+
			"statement_timeout. Any single SQL statement that runs longer than this "+
			"is cancelled by Postgres and the savepoint catches it as a handler "+
			"error (no silent stall). 0 = disabled.")

	fs.BoolVar(&cfg.MirrorRaw, "mirror-raw", envOrBool("SYNC_STATE_MIRROR_RAW", true),
		"Mirror raw block/tx/event/attribute rows to sync_state.raw_*. "+
			"Default true since the Phase B cache.* compatibility views (webapp reads "+
			"cache.blocks / cache.tx_results / cache.events / cache.attributes) read "+
			"from these. Disable only for short backfill runs where the views are unused.")
	fs.BoolVar(&cfg.StrictUnknownEvents, "strict-unknown-events", envOrBool("SYNC_STATE_STRICT_UNKNOWN", false),
		"Make unknown composite_keys fatal instead of counted-and-skipped")
	fs.BoolVar(&cfg.SkipDoctor, "skip-doctor", envOrBool("SYNC_STATE_SKIP_DOCTOR", false),
		"Skip the doctor checks at startup (CI helper; lock is always acquired)")
	fs.BoolVar(&cfg.TipWebsocket, "tip-ws", envOrBool("SYNC_STATE_TIP_WS", true),
		"Subscribe to CometBFT NewBlock WebSocket pushes so tip-idle wakes "+
			"immediately on commit (default true). Set false to fall back to "+
			"pure polling -- useful when the node's /websocket isn't exposed.")

	// verify
	fs.BoolVar(&cfg.VerifyErrorsOnly, "errors-only", false,
		"verify: run only the handler_errors_unresolved check")
	fs.BoolVar(&cfg.VerifyWriteReport, "write-report", true,
		"verify: persist results to sync_state.verification_report")
	fs.BoolVar(&cfg.VerifyJSON, "json", false,
		"verify: emit results as JSON instead of text")
	fs.Int64Var(&cfg.VerifyLagWarn, "lag-warn", 5,
		"verify: lag_blocks above which current_block_status FAILs")

	// init-genesis
	fs.StringVar(&cfg.GenesisFile, "genesis-file", envOr("SYNC_STATE_GENESIS_FILE", ""),
		"init-genesis: load genesis JSON from this local file instead of "+
			"fetching via RPC /genesis[_chunked]. Useful for air-gapped imports.")
	fs.BoolVar(&cfg.ForceGenesis, "force", envOrBool("SYNC_STATE_GENESIS_FORCE", false),
		"init-genesis: re-apply even when sync_state.genesis_log already has "+
			"a row for this chain (DELETE+INSERT is always replay-safe).")
	fs.BoolVar(&cfg.GenesisAutoApply, "genesis-auto-apply", envOrBool("SYNC_STATE_GENESIS_AUTO_APPLY", true),
		"ingest: when start-height=1 and no genesis_log row exists, auto-run "+
			"init-genesis before the first block. Set false to require an "+
			"explicit 'sync-state init-genesis' (operators wanting tight control).")

	fs.BoolVar(&cfg.BulkEnabled, "bulk-enabled", envOrBool("SYNC_STATE_BULK_ENABLED", true),
		"ingest: when lag >= bulk-lag-threshold, commit every bulk-window blocks "+
			"in one transaction instead of one tx per block (catch-up only)")
	fs.IntVar(&cfg.BulkWindow, "bulk-window", envOrInt("SYNC_STATE_BULK_WINDOW", 100),
		"ingest: max blocks per bulk outer transaction (caps fetch window when bulk is active)")
	fs.IntVar(&cfg.BulkLagThreshold, "bulk-lag-threshold", envOrInt("SYNC_STATE_BULK_LAG_THRESHOLD", 50),
		"ingest: switch to bulk mode when tip - cursor >= this many blocks")
	fs.DurationVar(&cfg.BulkStatementTimeout, "bulk-statement-timeout", envOrDuration("SYNC_STATE_BULK_STATEMENT_TIMEOUT", 5*time.Minute),
		"ingest: SET LOCAL statement_timeout for bulk outer transactions (default 5m)")

	// reprocess-errors
	fs.StringVar(&cfg.ReprocessCompositeKey, "composite-key", "",
		"reprocess-errors: only replay rows with this composite_key (empty = all)")
	fs.StringVar(&cfg.ReprocessSeverity, "severity", "error",
		"reprocess-errors: 'error' (default), 'warn', or '' for both")
	fs.Int64Var(&cfg.ReprocessSince, "since", 0,
		"reprocess-errors: only replay rows at height >= since (0 = no lower bound)")
	fs.Int64Var(&cfg.ReprocessUntil, "until", 0,
		"reprocess-errors: only replay rows at height <= until (0 = no upper bound)")
	fs.IntVar(&cfg.ReprocessLimit, "limit", 100,
		"reprocess-errors: cap rows processed per run (safety; 0 = no cap)")
	fs.BoolVar(&cfg.ReprocessDryRun, "dry-run", false,
		"reprocess-errors: fetch and dispatch in a rolled-back tx; do not resolve rows")

	_ = fs.Parse(args)

	// Resolve the deprecated -rpc / STRUCTS_RPC_URL knob. If set AND no
	// -rpc-primary was provided, treat it as the primary so existing
	// deployments keep working unchanged; emit a one-line notice that
	// the subcommand surfaces at startup.
	if legacyRPC != "" {
		if cfg.RPCPrimary == "" {
			cfg.RPCPrimary = legacyRPC
		}
		cfg.RPCDeprecationNotice = "NOTICE: -rpc / STRUCTS_RPC_URL is deprecated; use -rpc-primary / STRUCTS_RPC_PRIMARY (+ optional -rpc-seed / STRUCTS_RPC_SEED). Treating legacy value as primary."
	}

	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = buildDatabaseURL()
	}
	if cfg.Parallelism <= 0 {
		cfg.Parallelism = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1
	}
	if cfg.DBMaxConns <= 0 {
		cfg.DBMaxConns = 4
	}
	if cfg.BulkWindow <= 0 {
		cfg.BulkWindow = 1
	}
	if cfg.BulkLagThreshold < 0 {
		cfg.BulkLagThreshold = 0
	}
	return cfg
}

// RPCURLs returns the ordered endpoint list for rpc.NewClient: primary
// first (if set and distinct from seed), then seed. Empty seed is
// allowed for tests that wire URLs manually; in production LoadConfig
// guarantees a non-empty seed via DefaultRPCSeed.
func (c Config) RPCURLs() []string {
	out := make([]string, 0, 2)
	primary := strings.TrimSpace(c.RPCPrimary)
	seed := strings.TrimSpace(c.RPCSeed)
	if primary != "" {
		out = append(out, primary)
	}
	if seed != "" && seed != primary {
		out = append(out, seed)
	}
	return out
}

// buildDatabaseURL prefers DATABASE_URL, otherwise composes one from PG* env
// vars. Honors PGSSLMODE for TLS.
func buildDatabaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	host := envOr("PGHOST", "localhost")
	port := envOr("PGPORT", "5432")
	user := envOr("PGUSER", "structs")
	db := envOr("PGDATABASE", "structs")
	pass := os.Getenv("PGPASSWORD")
	sslmode := envOr("PGSSLMODE", "disable")

	u := &url.URL{
		Scheme: "postgres",
		Host:   fmt.Sprintf("%s:%s", host, port),
		Path:   "/" + db,
	}
	if pass != "" {
		u.User = url.UserPassword(user, pass)
	} else {
		u.User = url.User(user)
	}
	q := u.Query()
	q.Set("sslmode", sslmode)
	u.RawQuery = q.Encode()
	return u.String()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envOrInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOrInt64(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envOrBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envOrDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
