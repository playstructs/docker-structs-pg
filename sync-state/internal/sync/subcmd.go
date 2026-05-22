package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"sync-state/internal/db"
	"sync-state/internal/doctor"
	"sync-state/internal/events"
	"sync-state/internal/genesis"
	"sync-state/internal/reprocess"
	"sync-state/internal/rpc"
	"sync-state/internal/verify"
)

// Subcommand is the verb the binary is invoked with: ingest (default),
// bootstrap, doctor, list-handlers, replay, rewind, verify, inspect,
// reprocess-errors. Phase 1 implements ingest, bootstrap, doctor, and
// list-handlers; the rest stub-out with an "available in later phase" error.
type Subcommand string

const (
	CmdIngest          Subcommand = "ingest"
	CmdBootstrap       Subcommand = "bootstrap"
	CmdDoctor          Subcommand = "doctor"
	CmdListHandlers    Subcommand = "list-handlers"
	CmdReplay          Subcommand = "replay"
	CmdRewind          Subcommand = "rewind"
	CmdVerify          Subcommand = "verify"
	CmdInspect         Subcommand = "inspect"
	CmdReprocessErrors Subcommand = "reprocess-errors"
	CmdInitGenesis     Subcommand = "init-genesis"
)

// SplitArgs separates the subcommand verb from the remaining args. If the
// first arg isn't a known verb, it's treated as ingest with the original
// arglist (so `sync-state -start 1` keeps working).
func SplitArgs(args []string) (Subcommand, []string) {
	if len(args) == 0 {
		return CmdIngest, nil
	}
	switch Subcommand(args[0]) {
	case CmdIngest, CmdBootstrap, CmdDoctor, CmdListHandlers,
		CmdReplay, CmdRewind, CmdVerify, CmdInspect, CmdReprocessErrors,
		CmdInitGenesis:
		return Subcommand(args[0]), args[1:]
	default:
		return CmdIngest, args
	}
}

// Dispatch runs the requested subcommand. Returns the exit code.
//
// All flags (including subcommand-specific ones) are defined in
// LoadConfig and read off Config, so subcommands don't fight the
// top-level flag parser.
func Dispatch(ctx context.Context, cmd Subcommand, cfg Config, stdout, stderr io.Writer) int {
	switch cmd {
	case CmdListHandlers:
		return runListHandlers(stdout)

	case CmdBootstrap:
		return runBootstrap(ctx, cfg, stderr)

	case CmdDoctor:
		return runDoctor(ctx, cfg, stdout, stderr)

	case CmdIngest:
		return runIngest(ctx, cfg, stderr)

	case CmdVerify:
		return runVerify(ctx, cfg, stdout, stderr)

	case CmdReprocessErrors:
		return runReprocessErrors(ctx, cfg, stdout, stderr)

	case CmdInitGenesis:
		return runInitGenesis(ctx, cfg, stderr)

	case CmdReplay, CmdRewind, CmdInspect:
		fmt.Fprintf(stderr, "subcommand %q is planned for a later phase; not implemented in this build\n", cmd)
		return 2

	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", cmd)
		return 2
	}
}

// --- subcommand implementations -------------------------------------------

func runListHandlers(w io.Writer) int {
	router := events.NewRouter(false)
	handlers := router.Handlers()
	fmt.Fprintf(w, "%d registered event handlers:\n", len(handlers))
	for _, h := range handlers {
		fmt.Fprintf(w, "  %s\n", h.CompositeKey())
	}
	if len(handlers) == 0 {
		fmt.Fprintln(w, "(none in this build — Phases 2-5 add the routing handlers)")
	}
	return 0
}

func runBootstrap(ctx context.Context, cfg Config, stderr io.Writer) int {
	// Bootstrap doesn't need RPC; it only touches the DB. Skip the doctor.
	// We still need a chain_id to scope the writer lock; use "bootstrap"
	// as a stable placeholder so this subcommand doesn't collide with a
	// running ingest.
	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, "bootstrap")
	if err != nil {
		fmt.Fprintf(stderr, "bootstrap: db connect: %v\n", err)
		return 1
	}
	defer pool.Close()

	if err := db.Bootstrap(ctx, pool.Pool); err != nil {
		fmt.Fprintf(stderr, "bootstrap: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "sync_state schema + tables ensured (idempotent)")
	return 0
}

// runVerify wires the verify subcommand. Opens a pool (acquires the
// writer lock so two verifiers don't fight an ingester on the same chain
// — important for the planet_activity_seq queries which lock pages), then
// hands off to verify.Run.
func runVerify(ctx context.Context, cfg Config, stdout, stderr io.Writer) int {
	client := rpc.NewClient(cfg.RPCURLs(), cfg.HTTPTimeout, cfg.HTTPMaxRetries)
	if dep := cfg.RPCDeprecationNotice; dep != "" {
		fmt.Fprintln(stderr, dep)
	}

	// chain_id from RPC (same dance as doctor). Failing to reach RPC
	// isn't fatal; verify can still run the DB-only checks against the
	// cursor's chain_id. We pick that up as fallback.
	var chainID string
	if status, err := client.Status(ctx); err == nil {
		chainID = status.NodeInfo.Network
	} else {
		fmt.Fprintf(stderr, "verify: rpc /status unavailable, falling back to cursor chain_id: %v\n", err)
	}

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, lockChainID(chainID, "verify"))
	if err != nil {
		if errors.Is(err, db.ErrWriterLocked) {
			fmt.Fprintf(stderr, "verify: %v (another sync-state owns the writer lock)\n", err)
		} else {
			fmt.Fprintf(stderr, "verify: db connect: %v\n", err)
		}
		return 1
	}
	defer pool.Close()

	if chainID == "" {
		chainID = recoverChainIDFromCursor(ctx, pool)
		if chainID == "" {
			fmt.Fprintln(stderr, "verify: no chain_id from RPC and no cursor row yet; nothing to check")
			return 1
		}
	}

	return verify.Run(ctx, verify.CmdInputs{
		Pool:        pool.Pool,
		RPC:         client,
		ChainID:     chainID,
		MirrorRaw:   cfg.MirrorRaw,
		ErrorsOnly:  cfg.VerifyErrorsOnly,
		WriteReport: cfg.VerifyWriteReport,
		JSON:        cfg.VerifyJSON,
		LagWarn:     cfg.VerifyLagWarn,
	}, stdout, stderr)
}

// runReprocessErrors wires the reprocess-errors subcommand. Hands off to
// reprocess.Run after acquiring the writer lock (because replayed handlers
// will write to the same tables an active ingester would).
func runReprocessErrors(ctx context.Context, cfg Config, stdout, stderr io.Writer) int {
	client := rpc.NewClient(cfg.RPCURLs(), cfg.HTTPTimeout, cfg.HTTPMaxRetries)
	if dep := cfg.RPCDeprecationNotice; dep != "" {
		fmt.Fprintln(stderr, dep)
	}

	status, err := client.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "reprocess-errors: rpc /status: %v\n", err)
		return 1
	}
	chainID := status.NodeInfo.Network

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, chainID)
	if err != nil {
		if errors.Is(err, db.ErrWriterLocked) {
			fmt.Fprintf(stderr, "reprocess-errors: %v -- another sync-state owns the writer lock\n", err)
		} else {
			fmt.Fprintf(stderr, "reprocess-errors: db connect: %v\n", err)
		}
		return 1
	}
	defer pool.Close()

	router := events.NewRouter(cfg.StrictUnknownEvents)
	return reprocess.Run(ctx, reprocess.CmdInputs{
		Pool:         pool.Pool,
		RPC:          client,
		Router:       router,
		ChainID:      chainID,
		CompositeKey: cfg.ReprocessCompositeKey,
		Severity:     cfg.ReprocessSeverity,
		Since:        cfg.ReprocessSince,
		Until:        cfg.ReprocessUntil,
		Limit:        cfg.ReprocessLimit,
		DryRun:       cfg.ReprocessDryRun,
		MirrorRaw:    cfg.MirrorRaw,
	}, stdout, stderr)
}

// runInitGenesis is the `sync-state init-genesis` entry point: opens
// the DB, ensures bootstrap is current, fetches the genesis JSON
// (RPC by default, file via -genesis-file), and applies it. Refuses
// to re-apply unless -force is set so a stray re-run doesn't blow away
// post-genesis edits an operator made by hand (rare but possible during
// chain upgrade windows).
func runInitGenesis(ctx context.Context, cfg Config, stderr io.Writer) int {
	client := rpc.NewClient(cfg.RPCURLs(), cfg.HTTPTimeout, cfg.HTTPMaxRetries)
	if dep := cfg.RPCDeprecationNotice; dep != "" {
		fmt.Fprintln(stderr, dep)
	}

	// Need chain_id for the lock + genesis_log row. RPC is the
	// authoritative source even when -genesis-file is set; that way a
	// mistakenly-loaded mainnet genesis against a testnet DB is caught
	// before any rows are written. Skipped only when RPC is unreachable
	// AND we have a -genesis-file (fully air-gapped import).
	var chainID string
	status, err := client.Status(ctx)
	if err != nil {
		if cfg.GenesisFile == "" {
			fmt.Fprintf(stderr, "init-genesis: rpc /status: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "init-genesis: rpc /status unavailable (%v); will use chain_id from genesis file\n", err)
	} else {
		chainID = status.NodeInfo.Network
	}

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, lockChainID(chainID, "init-genesis"))
	if err != nil {
		if errors.Is(err, db.ErrWriterLocked) {
			fmt.Fprintf(stderr, "init-genesis: %v (another sync-state owns the writer lock)\n", err)
		} else {
			fmt.Fprintf(stderr, "init-genesis: db connect: %v\n", err)
		}
		return 1
	}
	defer pool.Close()

	if err := db.Bootstrap(ctx, pool.Pool); err != nil {
		fmt.Fprintf(stderr, "init-genesis: bootstrap: %v\n", err)
		return 1
	}

	loaded, err := loadGenesisDocument(ctx, cfg, client, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "init-genesis: %v\n", err)
		return 1
	}
	if chainID != "" && loaded.Doc.ChainID != chainID {
		fmt.Fprintf(stderr, "init-genesis: REFUSING: genesis chain_id=%s != RPC chain_id=%s\n",
			loaded.Doc.ChainID, chainID)
		return 1
	}
	if chainID == "" {
		chainID = loaded.Doc.ChainID
	}

	existing, err := db.ReadGenesisLog(ctx, pool.Pool, chainID)
	if err != nil {
		fmt.Fprintf(stderr, "init-genesis: read genesis_log: %v\n", err)
		return 1
	}
	if existing != nil && !cfg.ForceGenesis {
		fmt.Fprintf(stderr, "init-genesis: already applied for chain=%s (applied_at=%s, total_rows=%d, sha256=%s).\n",
			chainID, existing.AppliedAt.UTC().Format("2006-01-02T15:04:05Z"),
			existing.TotalRows, existing.SHA256)
		fmt.Fprintln(stderr, "  Pass -force to re-apply (DELETE FROM structs.ledger WHERE action='genesis' first).")
		return 0
	}

	report, err := genesis.Apply(ctx, pool.Pool, loaded)
	if err != nil {
		fmt.Fprintf(stderr, "init-genesis: apply: %v\n", err)
		return 1
	}
	printApplyReport(stderr, report)
	return 0
}

// loadGenesisDocument is the shared loader: -genesis-file wins when
// set, otherwise RPC. Surfaced as a helper so the auto-apply path in
// runIngest can reuse the same dispatch logic.
func loadGenesisDocument(ctx context.Context, cfg Config, client *rpc.Client, stderr io.Writer) (*genesis.LoadedDocument, error) {
	if cfg.GenesisFile != "" {
		fmt.Fprintf(stderr, "init-genesis: loading from file %s\n", cfg.GenesisFile)
		return genesis.LoadFromFile(cfg.GenesisFile)
	}
	fmt.Fprintf(stderr, "init-genesis: fetching from RPC (%s + chunked fallback)\n", client.BaseURL())
	return genesis.LoadFromRPC(ctx, client)
}

// printApplyReport formats the post-apply summary on stderr. Same
// format whether init-genesis was invoked directly or auto-applied by
// runIngest so operators get the same visibility either way.
func printApplyReport(w io.Writer, r *genesis.ApplyReport) {
	fmt.Fprintf(w, "init-genesis: applied chain=%s in %s (deleted %d prior genesis rows)\n",
		r.LogRow.ChainID, r.Elapsed.Round(1e6), r.PreDeleteRows)
	fmt.Fprintf(w, "  source         = %s\n", r.LogRow.Source)
	fmt.Fprintf(w, "  genesis_time   = %s\n", r.LogRow.GenesisTime.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(w, "  sha256         = %s\n", r.LogRow.SHA256)
	fmt.Fprintf(w, "  rows_inserted  = %d total\n", r.LogRow.TotalRows)
	for _, k := range []string{"bank", "delegations", "unbondings", "ore"} {
		fmt.Fprintf(w, "    %-12s = %d\n", k, r.LogRow.RowsPerSection[k])
	}
}

// lockChainID returns chainID when non-empty, otherwise a stable per-verb
// fallback so the advisory lock keys don't collide between subcommands.
func lockChainID(chainID, fallbackSuffix string) string {
	if chainID != "" {
		return chainID
	}
	return "sync-state-" + fallbackSuffix
}

// recoverChainIDFromCursor reads the most recently used chain_id from
// sync_state.sync_cursor. Used as a fallback when RPC is unreachable.
func recoverChainIDFromCursor(ctx context.Context, pool *db.Pool) string {
	var id string
	err := pool.Pool.QueryRow(ctx, `
		SELECT chain_id FROM sync_state.sync_cursor
		 ORDER BY updated_at DESC LIMIT 1
	`).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

func runDoctor(ctx context.Context, cfg Config, stdout, stderr io.Writer) int {
	client := rpc.NewClient(cfg.RPCURLs(), cfg.HTTPTimeout, cfg.HTTPMaxRetries)

	if dep := cfg.RPCDeprecationNotice; dep != "" {
		fmt.Fprintln(stderr, dep)
	}

	// Pre-flight: we need the chain_id from /status to scope the lock, but
	// we want to run RPC-only doctor probes too. So: get /status first
	// (without a lock), then open the pool + lock if chain_id is known.
	status, err := client.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: rpc /status: %v\n", err)
		return 1
	}
	chainID := status.NodeInfo.Network

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, chainID)
	if err != nil {
		if errors.Is(err, db.ErrWriterLocked) {
			fmt.Fprintf(stderr, "doctor: %v (another sync-state owns the writer lock)\n", err)
		} else {
			fmt.Fprintf(stderr, "doctor: db connect: %v\n", err)
		}
		return 1
	}
	defer pool.Close()

	report, err := doctor.Run(ctx, doctor.Inputs{
		RPC:                 client,
		Pool:                pool.Pool,
		ExpectedChainID:     chainID,
		StartHeight:         cfg.StartHeight,
		TipWebsocketEnabled: cfg.TipWebsocket,
	})
	if err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return 1
	}
	report.Print(stdout)
	if report.Fatal {
		return 1
	}
	return 0
}

func runIngest(ctx context.Context, cfg Config, stderr io.Writer) int {
	client := rpc.NewClient(cfg.RPCURLs(), cfg.HTTPTimeout, cfg.HTTPMaxRetries)

	if dep := cfg.RPCDeprecationNotice; dep != "" {
		fmt.Fprintln(stderr, dep)
	}
	urls := client.URLs()
	fmt.Fprintf(stderr, "RPC pool (%d endpoint(s), primary first):\n", len(urls))
	for i, u := range urls {
		role := "primary"
		if i > 0 {
			role = "fallback"
		}
		fmt.Fprintf(stderr, "  [%d] %s (%s)\n", i, u, role)
	}

	status, err := client.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "rpc /status: %v\n", err)
		return 1
	}
	chainID := status.NodeInfo.Network
	tip := status.Latest()
	earliest := status.Earliest()
	fmt.Fprintf(stderr, "Connected: chain_id=%s tip=%d earliest=%d catching_up=%v\n",
		chainID, tip, earliest, status.SyncInfo.CatchingUp)

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, chainID)
	if err != nil {
		if errors.Is(err, db.ErrWriterLocked) {
			fmt.Fprintf(stderr, "%v -- another sync-state instance already owns the writer lock\n", err)
		} else {
			fmt.Fprintf(stderr, "db connect: %v\n", err)
		}
		return 1
	}
	defer pool.Close()

	// Bootstrap schema / column additions (idempotent).
	if err := db.Bootstrap(ctx, pool.Pool); err != nil {
		fmt.Fprintf(stderr, "bootstrap: %v\n", err)
		return 1
	}

	// Unresolved handler_error_log summary. Always show even when the
	// numbers are zero — operators learn what to look for. We only nag
	// (and surface replay/verify hints) when there's actually something
	// in the queue.
	if errCount, warnCount, err := db.UnresolvedErrorSummary(ctx, pool.Pool, chainID); err == nil {
		switch {
		case errCount+warnCount == 0:
			fmt.Fprintln(stderr, "Handler error log: 0 unresolved rows")
		default:
			fmt.Fprintf(stderr, "WARN: %d unresolved handler_error_log rows (%d error, %d warn).\n",
				errCount+warnCount, errCount, warnCount)
			fmt.Fprintln(stderr, "  Inspect with: sync-state verify --errors-only")
			fmt.Fprintln(stderr, "  Replay with:  sync-state reprocess-errors --since=<height>")
		}
	} else {
		fmt.Fprintf(stderr, "WARN: could not read handler_error_log summary: %v\n", err)
	}

	// Doctor (always runs; SkipDoctor only skips the RPC + dropped-trigger
	// probes, not the lock acquisition which already happened above).
	if !cfg.SkipDoctor {
		report, err := doctor.Run(ctx, doctor.Inputs{
			RPC:                 client,
			Pool:                pool.Pool,
			ExpectedChainID:     chainID,
			StartHeight:         cfg.StartHeight,
			TipWebsocketEnabled: cfg.TipWebsocket,
		})
		if err != nil {
			fmt.Fprintf(stderr, "doctor: %v\n", err)
			return 1
		}
		report.Print(os.Stderr)
		if report.Fatal {
			fmt.Fprintln(stderr, "refusing to start ingest: doctor reported FATAL")
			return 1
		}
	}

	// Reorg detection on resume.
	if err := CheckReorg(ctx, client, pool, chainID); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		fmt.Fprintln(stderr, "refusing to continue: run `sync-state rewind --to=H` to truncate to a safe height")
		return 1
	}

	start, err := ResolveStart(ctx, pool, chainID, cfg.StartHeight, earliest)
	if err != nil {
		fmt.Fprintf(stderr, "resolve start: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "Starting sync at height %d (chain=%s, node has [%d..%d])\n",
		start, chainID, earliest, tip)

	// Genesis import gate. There are three states:
	//   1. genesis_log has a row for this chain → nothing to do.
	//   2. No row AND start <= 1 AND cfg.GenesisAutoApply → load+apply now.
	//   3. No row AND (start > 1 OR auto-apply disabled) → loud warning,
	//      proceed anyway. The verify check will FAIL on this state so
	//      operators can't miss it forever, but we don't want to refuse
	//      ingest because some deployments load genesis out-of-band
	//      (e.g. archive-node re-syncs that already populated ledger).
	switch existing, gerr := db.ReadGenesisLog(ctx, pool.Pool, chainID); {
	case gerr != nil:
		fmt.Fprintf(stderr, "WARN: read genesis_log: %v (continuing)\n", gerr)
	case existing != nil:
		fmt.Fprintf(stderr, "Genesis: already applied at %s (total_rows=%d sha256=%s)\n",
			existing.AppliedAt.UTC().Format("2006-01-02T15:04:05Z"),
			existing.TotalRows, existing.SHA256)
	case start <= 1 && cfg.GenesisAutoApply:
		fmt.Fprintln(stderr, "Genesis: no genesis_log row and starting from height 1 — auto-applying.")
		loaded, lerr := loadGenesisDocument(ctx, cfg, client, stderr)
		if lerr != nil {
			fmt.Fprintf(stderr, "ingest: genesis load: %v\n", lerr)
			return 1
		}
		if loaded.Doc.ChainID != chainID {
			fmt.Fprintf(stderr, "ingest: REFUSING: genesis chain_id=%s != RPC chain_id=%s\n",
				loaded.Doc.ChainID, chainID)
			return 1
		}
		report, aerr := genesis.Apply(ctx, pool.Pool, loaded)
		if aerr != nil {
			fmt.Fprintf(stderr, "ingest: genesis apply: %v\n", aerr)
			return 1
		}
		printApplyReport(stderr, report)
	default:
		fmt.Fprintln(stderr, "WARN: no sync_state.genesis_log row for this chain.")
		fmt.Fprintln(stderr, "  Bank ledger balances will be missing genesis credits until")
		fmt.Fprintln(stderr, "  `sync-state init-genesis` runs. `sync-state verify` will FAIL")
		fmt.Fprintln(stderr, "  the genesis_loaded check on this state.")
	}

	router := events.NewRouter(cfg.StrictUnknownEvents)
	fmt.Fprintf(stderr, "Event registry: %d handlers (use `sync-state list-handlers` to inspect)\n",
		router.Count())

	syncer := NewSyncer(cfg, client, pool, router, chainID)

	// Optional NewBlock WebSocket push. Reduces tip-detection latency
	// from ~PollInterval to ~RTT. The notifier auto-reconnects and
	// fails over across the same endpoint list as the JSON-RPC client;
	// if everything is unreachable the syncer's poll interval kicks in
	// as a failsafe (so the worst case is still bounded by -poll).
	if cfg.TipWebsocket {
		notifier := rpc.NewTipNotifier(client.URLs(), log.New(stderr, "tip-ws: ", log.LstdFlags|log.Lmicroseconds))
		go notifier.Run(ctx)
		syncer.WithTipNotifier(notifier.C())
		fmt.Fprintf(stderr, "Tip WebSocket push enabled (poll %s = failsafe). Disable with -tip-ws=false.\n", cfg.PollInterval)
	} else {
		fmt.Fprintf(stderr, "Tip WebSocket push disabled; relying on poll interval %s.\n", cfg.PollInterval)
	}

	if cfg.StatementTimeout > 0 {
		fmt.Fprintf(stderr, "Per-statement timeout: %s (any single SQL stmt slower than this is cancelled and logged as a handler error).\n", cfg.StatementTimeout)
	} else {
		fmt.Fprintln(stderr, "Per-statement timeout: DISABLED (a hung query will stall ingest indefinitely).")
	}

	if err := syncer.Run(ctx, start); err != nil {
		if IsContextErr(err) {
			fmt.Fprintln(stderr, "Sync cancelled")
			return 0
		}
		fmt.Fprintf(stderr, "sync: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "Sync done")
	return 0
}
