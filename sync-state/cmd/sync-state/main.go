// Command sync-state ingests blocks from a CometBFT RPC endpoint into the
// local Structs PostgreSQL database. It replaces the SQL-trigger routing
// (`cache.add_queue`, `cache.process_block_ledger`, planet activity
// derivations, etc.) with Go-side dispatch.
//
// Subcommands:
//
//	sync-state                 # tip-follow ingest (default)
//	sync-state ingest          # explicit alias
//	sync-state bootstrap       # idempotent schema setup, then exit
//	sync-state doctor          # node compatibility check, then exit
//	sync-state list-handlers   # print the registered event handlers
//	sync-state verify          # run data-quality checks, write a report
//	sync-state reprocess-errors# replay unresolved handler_error_log rows
//
// See scripts/sync_state.sh for the env vars sync-state honours.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sync_pkg "sync-state/internal/sync"
)

func main() {
	args := os.Args[1:]
	cmd, rest := sync_pkg.SplitArgs(args)
	cfg := sync_pkg.LoadConfig(rest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		fmt.Fprintf(os.Stderr, "Received signal %s, shutting down...\n", s)
		cancel()
	}()

	code := sync_pkg.Dispatch(ctx, cmd, cfg, os.Stdout, os.Stderr)
	os.Exit(code)
}
