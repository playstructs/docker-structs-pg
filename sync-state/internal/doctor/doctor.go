// Package doctor probes the upstream CometBFT node and the local Postgres
// DB before sync-state starts ingesting, to catch operator misconfigurations
// early (snapshot-synced node, discard_abci_responses=true, tx indexer
// disabled, trigger-vs-flag mismatch, concurrent writer, etc).
//
// Run automatically at startup and exposed as `sync-state doctor` for
// ad-hoc operator use.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/rpc"
)

// Severity classifies one check's outcome.
type Severity int

const (
	OK Severity = iota
	WARN
	FATAL
)

func (s Severity) String() string {
	switch s {
	case OK:
		return "OK"
	case WARN:
		return "WARN"
	case FATAL:
		return "FATAL"
	}
	return "?"
}

// Check is one named probe result.
type Check struct {
	Name     string
	Severity Severity
	Detail   string
}

// EndpointReport captures the per-URL slice of probes used to validate
// that an RPC pool endpoint is usable. The doctor builds one of these
// per configured URL so operators can see exactly which endpoints are
// healthy and why a pool-level verdict came out the way it did.
type EndpointReport struct {
	URL      string
	Role     string // "primary" or "fallback"
	Reached  bool   // did /status succeed?
	ChainID  string
	Tip      int64
	Earliest int64
	Checks   []Check
}

// Report is the full output of a doctor run.
type Report struct {
	RPCURL    string // preferred endpoint URL (for legacy single-line header)
	ChainID   string // populated even on partial failure if we got /status
	Tip       int64
	Earliest  int64

	// Endpoints holds per-URL probe results. Always populated; for a
	// single-URL config it has one entry whose role is "primary".
	Endpoints []EndpointReport

	// Checks are the chain-wide / DB-wide probes that aren't tied to a
	// specific RPC URL (trigger-vs-flag, update-cache concurrency, the
	// summary node-liveness/coverage rolls). Per-endpoint findings live
	// in Endpoints[*].Checks instead.
	Checks  []Check
	Verdict string
	Fatal   bool // any FATAL among Checks or per-endpoint Checks
}

// Inputs are the knobs the caller passes in. The caller resolves chain_id /
// start_height etc. before calling Run.
type Inputs struct {
	RPC                 *rpc.Client
	Pool                *pgxpool.Pool
	ExpectedChainID     string
	StartHeight         int64
	SkipCacheConcurrent bool // skip the "update-cache wrote recently" probe (e.g. in tests)

	// TipWebsocketEnabled lets the doctor know whether to probe each
	// endpoint's /websocket reachability. When the operator turned
	// push off (-tip-ws=false), the probe is skipped so we don't WARN
	// about a feature they explicitly disabled.
	TipWebsocketEnabled bool
}

// Run executes every check and returns the report. Does not exit the
// process; the caller decides what to do with FATAL outcomes.
func Run(ctx context.Context, in Inputs) (*Report, error) {
	r := &Report{
		RPCURL: in.RPC.BaseURL(),
	}

	// 1. Per-endpoint probes. For each URL in the pool we collect
	// chain_id, tip, catching_up, earliest, /block at earliest, tx
	// indexer. Pool-level invariants (chain_id consistency, at-least-one
	// reachable) are validated after.
	urls := in.RPC.URLs()
	if len(urls) == 0 {
		r.Checks = append(r.Checks, Check{
			Name:     "rpc pool",
			Severity: FATAL,
			Detail:   "no rpc endpoints configured (set -rpc-primary or -rpc-seed)",
		})
		r.Fatal = true
		r.Verdict = "FATAL: empty rpc pool"
		return r, nil
	}
	for i, u := range urls {
		role := "primary"
		if i > 0 {
			role = "fallback"
		}
		ep := probeEndpoint(ctx, in.RPC, u, role)
		if in.TipWebsocketEnabled {
			ep.Checks = append(ep.Checks, probeWebsocketReachable(ctx, u))
		}
		r.Endpoints = append(r.Endpoints, ep)
	}

	// 2. Pool-level checks: at least one endpoint reached, chain_ids
	// consistent (mismatch = wrong network wired up).
	healthy, chainSet := poolStats(r.Endpoints)
	if healthy == 0 {
		r.Checks = append(r.Checks, Check{
			Name:     "rpc pool",
			Severity: FATAL,
			Detail:   "no endpoints reachable",
		})
		r.Fatal = true
		r.Verdict = "FATAL: cannot reach any RPC node"
		return r, nil
	}
	if len(chainSet) > 1 {
		r.Checks = append(r.Checks, Check{
			Name:     "rpc pool",
			Severity: FATAL,
			Detail:   fmt.Sprintf("endpoints disagree on chain_id: %v (operator wired the wrong network)", chainSet),
		})
		r.Fatal = true
		r.Verdict = "FATAL: chain_id mismatch across pool"
		return r, nil
	}
	if len(r.Endpoints) > 1 && healthy < len(r.Endpoints) {
		r.Checks = append(r.Checks, Check{
			Name:     "rpc pool",
			Severity: WARN,
			Detail:   fmt.Sprintf("%d of %d endpoints healthy; sync will proceed against reachable endpoints", healthy, len(r.Endpoints)),
		})
	} else {
		r.Checks = append(r.Checks, Check{
			Name:     "rpc pool",
			Severity: OK,
			Detail:   fmt.Sprintf("%d/%d endpoint(s) healthy, chain_id=%s", healthy, len(r.Endpoints), firstKey(chainSet)),
		})
	}

	// Pick the first reached endpoint as the authoritative source for
	// chain-wide checks (tip, earliest, coverage probes). This is the
	// same endpoint sync-state will see as preferred.
	auth := authoritativeEndpoint(r.Endpoints)
	r.ChainID = auth.ChainID
	r.Tip = auth.Tip
	r.Earliest = auth.Earliest

	if in.ExpectedChainID != "" && r.ChainID != in.ExpectedChainID {
		r.Checks = append(r.Checks, Check{
			Name:     "chain_id",
			Severity: FATAL,
			Detail:   fmt.Sprintf("pool reports %q but expected %q", r.ChainID, in.ExpectedChainID),
		})
		r.Fatal = true
		r.Verdict = "FATAL: chain_id mismatch"
		return r, nil
	}

	// 3. earliest_block_height — informational + WARN if non-archive
	archiveSev := OK
	archiveDetail := fmt.Sprintf("earliest=%d (archive node)", r.Earliest)
	if r.Earliest > 1 {
		archiveSev = WARN
		archiveDetail = fmt.Sprintf("earliest=%d (state-synced or pruned; pre-earliest history unavailable)", r.Earliest)
	}
	r.Checks = append(r.Checks, Check{Name: "earliest_block_height", Severity: archiveSev, Detail: archiveDetail})

	// 4. /block at earliest
	if _, err := in.RPC.Block(ctx, r.Earliest); err != nil {
		r.Checks = append(r.Checks, Check{
			Name:     "block store at earliest",
			Severity: FATAL,
			Detail:   err.Error(),
		})
		r.Fatal = true
	} else {
		r.Checks = append(r.Checks, Check{
			Name:     "block store at earliest",
			Severity: OK,
			Detail:   fmt.Sprintf("/block?height=%d returned OK", r.Earliest),
		})
	}

	// 5. /block_results at earliest
	if _, err := in.RPC.BlockResults(ctx, r.Earliest); err != nil {
		r.Checks = append(r.Checks, Check{
			Name:     "state store at earliest",
			Severity: FATAL,
			Detail:   err.Error(),
		})
		r.Fatal = true
	} else {
		r.Checks = append(r.Checks, Check{
			Name:     "state store at earliest",
			Severity: OK,
			Detail:   fmt.Sprintf("/block_results?height=%d returned OK", r.Earliest),
		})
	}

	// 6. coverage at requested start (informational if start <= earliest)
	if in.StartHeight > 0 && in.StartHeight >= r.Earliest && in.StartHeight <= r.Tip {
		if _, err := in.RPC.BlockResults(ctx, in.StartHeight); err != nil {
			r.Checks = append(r.Checks, Check{
				Name:     fmt.Sprintf("coverage at start=%d", in.StartHeight),
				Severity: FATAL,
				Detail:   err.Error(),
			})
			r.Fatal = true
		} else {
			r.Checks = append(r.Checks, Check{
				Name:     fmt.Sprintf("coverage at start=%d", in.StartHeight),
				Severity: OK,
				Detail:   "OK",
			})
		}
	}

	// 7. coverage at tip
	if _, err := in.RPC.BlockResults(ctx, r.Tip); err != nil {
		r.Checks = append(r.Checks, Check{
			Name:     "coverage at tip",
			Severity: FATAL,
			Detail:   err.Error(),
		})
		r.Fatal = true
	} else {
		r.Checks = append(r.Checks, Check{
			Name:     "coverage at tip",
			Severity: OK,
			Detail:   "OK",
		})
	}

	// 8. discard_abci_responses heuristic — find a block with txs and
	// confirm its results have tx_results rows.
	if c := probeABCIRetention(ctx, in.RPC, r.Tip); c != nil {
		r.Checks = append(r.Checks, *c)
		if c.Severity == FATAL {
			r.Fatal = true
		}
	}

	// 9. tx indexer
	if c := probeTxIndexer(ctx, in.RPC); c != nil {
		r.Checks = append(r.Checks, *c)
		if c.Severity == FATAL {
			r.Fatal = true
		}
	}

	// 10. cache-trigger absence: sync-state now owns every derivation
	// the cache.* triggers used to handle. Any still-enabled cache-era
	// trigger means we'd double-write on the next block.
	if in.Pool != nil {
		if c := probeDroppedTriggers(ctx, in.Pool); c != nil {
			r.Checks = append(r.Checks, *c)
			if c.Severity == FATAL {
				r.Fatal = true
			}
		}

		// 11. canonical schema: assert that the columns the Phase B SQL
		// added natively (current_block.status/lag_blocks/tip_height,
		// planet_activity.block_height, stat_*.block_height) are present.
		// Without them sync-state's INSERTs would fail with column-not-found
		// on the first commit. Earlier sync-state releases bootstrapped
		// these via ALTER at startup; that path was removed once
		// retire-cache.sql became the source of truth.
		if c := probeCanonicalSchema(ctx, in.Pool); c != nil {
			r.Checks = append(r.Checks, *c)
			if c.Severity == FATAL {
				r.Fatal = true
			}
		}

		// 12. concurrent update-cache heuristic
		if !in.SkipCacheConcurrent {
			if c := probeConcurrentUpdateCache(ctx, in.Pool); c != nil {
				r.Checks = append(r.Checks, *c)
				// only WARN ever; doesn't escalate
			}
		}
	}

	// Verdict.
	switch {
	case r.Fatal:
		r.Verdict = "FATAL: cannot start; see failed checks above"
	case r.Earliest > 1:
		r.Verdict = fmt.Sprintf("NON-ARCHIVE NODE. sync-state will clamp SYNC_START_HEIGHT to %d.", r.Earliest)
	default:
		r.Verdict = "ARCHIVE NODE, suitable for full backfill from height 1."
	}
	return r, nil
}

// probeEndpoint runs the per-URL slice of probes: chain_id (via
// /status), tip + catching_up, earliest, and tx indexer. Doesn't escalate
// to FATAL on its own — pool-level rules in Run() decide that based on
// how many endpoints are reachable and whether their chain_ids agree.
//
// The /status call is bounded by a short context deadline so a host
// that's dropping packets doesn't stall startup. The pool's chosen
// preferred endpoint will get the full retry budget on actual block
// fetches; here we only need a quick liveness signal.
func probeEndpoint(ctx context.Context, c *rpc.Client, url, role string) EndpointReport {
	er := EndpointReport{URL: url, Role: role}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := c.StatusOf(probeCtx, url)
	if err != nil {
		er.Checks = append(er.Checks, Check{
			Name:     "reachable",
			Severity: FATAL,
			Detail:   fmt.Sprintf("/status failed: %v", err),
		})
		return er
	}
	er.Reached = true
	er.ChainID = status.NodeInfo.Network
	er.Tip = status.Latest()
	er.Earliest = status.Earliest()

	er.Checks = append(er.Checks, Check{
		Name:     "chain_id",
		Severity: OK,
		Detail:   er.ChainID,
	})

	if status.SyncInfo.CatchingUp {
		er.Checks = append(er.Checks, Check{
			Name:     "node liveness",
			Severity: WARN,
			Detail:   fmt.Sprintf("catching_up=true at tip=%d (sync-state will skip this endpoint for blocks above its tip until it caches up)", er.Tip),
		})
	} else {
		er.Checks = append(er.Checks, Check{
			Name:     "node liveness",
			Severity: OK,
			Detail:   fmt.Sprintf("tip=%d catching_up=false", er.Tip),
		})
	}

	if er.Earliest > 1 {
		er.Checks = append(er.Checks, Check{
			Name:     "earliest_block_height",
			Severity: WARN,
			Detail:   fmt.Sprintf("earliest=%d (state-synced or pruned; pre-earliest history unavailable from this endpoint)", er.Earliest),
		})
	} else {
		er.Checks = append(er.Checks, Check{
			Name:     "earliest_block_height",
			Severity: OK,
			Detail:   fmt.Sprintf("earliest=%d (archive)", er.Earliest),
		})
	}
	return er
}

// probeWebsocketReachable attempts an Upgrade against the endpoint's
// /websocket path. The handshake either succeeds (push-notifier will
// work against this host) or fails (the operator should expect
// poll-only tip detection for this endpoint and the failsafe -poll
// interval becomes the floor for tip latency).
//
// This is intentionally a single-attempt probe with a short deadline;
// the runtime notifier has its own backoff + endpoint-rotation logic.
func probeWebsocketReachable(ctx context.Context, raw string) Check {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return Check{
			Name:     "tip websocket",
			Severity: WARN,
			Detail:   fmt.Sprintf("unparseable URL %q: %v (push notifier will be skipped)", raw, err),
		}
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	if !strings.HasSuffix(u.Path, "/websocket") {
		u.Path = strings.TrimRight(u.Path, "/") + "/websocket"
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, resp, err := dialer.DialContext(dialCtx, u.String(), nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return Check{
			Name:     "tip websocket",
			Severity: WARN,
			Detail: fmt.Sprintf("upgrade to %s failed (http=%d): %v -- tip-push disabled for this host; "+
				"poll-interval failsafe will be used", u.String(), code, err),
		}
	}
	_ = conn.Close()
	return Check{
		Name:     "tip websocket",
		Severity: OK,
		Detail:   fmt.Sprintf("%s reachable; NewBlock push will be subscribed at runtime", u.String()),
	}
}

// poolStats counts reachable endpoints and tallies the distinct
// chain_ids reported across the pool. A pool with >1 chain_id means the
// operator wired an endpoint pointing at the wrong network — FATAL.
func poolStats(eps []EndpointReport) (healthy int, chainIDs map[string]int) {
	chainIDs = map[string]int{}
	for _, e := range eps {
		if !e.Reached {
			continue
		}
		healthy++
		if e.ChainID != "" {
			chainIDs[e.ChainID]++
		}
	}
	return healthy, chainIDs
}

// authoritativeEndpoint picks the first reached endpoint (preference
// order is preserved) to serve as the source of truth for chain-wide
// metadata (tip, earliest). When the primary is up we use it; otherwise
// we fall to the next healthy fallback.
func authoritativeEndpoint(eps []EndpointReport) EndpointReport {
	for _, e := range eps {
		if e.Reached {
			return e
		}
	}
	return EndpointReport{}
}

// firstKey returns one key from m for embedding in a status string. Map
// iteration order is unspecified; we only call this once chain_id
// consistency has been validated, so any key is representative.
func firstKey(m map[string]int) string {
	for k := range m {
		return k
	}
	return ""
}

// probeABCIRetention finds a recent block whose /block has at least one tx,
// then checks /block_results.txs_results for that block to detect
// discard_abci_responses=true.
func probeABCIRetention(ctx context.Context, c *rpc.Client, tip int64) *Check {
	const scanWindow = 30
	from := tip - scanWindow + 1
	if from < 2 {
		from = 2
	}
	bc, err := c.Blockchain(ctx, from, tip)
	if err != nil {
		return &Check{
			Name:     "abci retention probe",
			Severity: WARN,
			Detail:   fmt.Sprintf("/blockchain failed: %v (skipping)", err),
		}
	}
	var sampleHeight int64
	for _, m := range bc.BlockMetas {
		n, _ := strconv.Atoi(m.NumTxs)
		if n > 0 {
			h, _ := strconv.ParseInt(m.Header.Height, 10, 64)
			sampleHeight = h
			break
		}
	}
	if sampleHeight == 0 {
		return &Check{
			Name:     "abci retention probe",
			Severity: WARN,
			Detail:   fmt.Sprintf("no tx-bearing blocks in last %d heights; cannot probe (skipping)", scanWindow),
		}
	}
	br, err := c.BlockResults(ctx, sampleHeight)
	if err != nil {
		return &Check{
			Name:     "abci retention probe",
			Severity: FATAL,
			Detail:   fmt.Sprintf("/block_results?height=%d failed: %v", sampleHeight, err),
		}
	}
	if len(br.TxsResults) == 0 {
		return &Check{
			Name:     "abci retention probe",
			Severity: FATAL,
			Detail:   fmt.Sprintf("block %d has txs but block_results.txs_results is empty (discard_abci_responses=true?)", sampleHeight),
		}
	}
	return &Check{
		Name:     "abci retention probe",
		Severity: OK,
		Detail:   fmt.Sprintf("block %d has txs and %d tx_results (discard_abci_responses=false)", sampleHeight, len(br.TxsResults)),
	}
}

// probeTxIndexer hits /tx_search with a trivial query. Indexer="null" returns
// an error envelope; "kv" and "psql" both return a normal envelope.
func probeTxIndexer(ctx context.Context, c *rpc.Client) *Check {
	r, err := c.TxSearch(ctx, "tx.height>=1", 1)
	if err != nil {
		// distinguish "indexer disabled" (deterministic rpc error mentioning indexer)
		// from transient failure
		if errors.Is(err, rpc.ErrRPCDeterministic) && strings.Contains(strings.ToLower(err.Error()), "index") {
			return &Check{
				Name:     "tx indexer",
				Severity: FATAL,
				Detail:   fmt.Sprintf("indexer disabled (%v); set [tx_index] indexer = \"kv\" in config.toml", err),
			}
		}
		return &Check{
			Name:     "tx indexer",
			Severity: WARN,
			Detail:   fmt.Sprintf("/tx_search failed transiently: %v", err),
		}
	}
	return &Check{
		Name:     "tx indexer",
		Severity: OK,
		Detail:   fmt.Sprintf("tx_search returned %d hits", len(r.Txs)),
	}
}

// droppedTriggers is the canonical list of cache-era triggers that
// sync-state now owns. The Phase B SQL drops them; if any is still
// enabled, the doctor FATALs because every block would double-write.
var droppedTriggers = []struct{ Schema, Table, Trigger string }{
	{Schema: "structs", Table: "struct", Trigger: "planet_activity_struct_movement"},
	{Schema: "structs", Table: "fleet", Trigger: "planet_activity_fleet_move"},
	{Schema: "structs", Table: "planet_raid", Trigger: "planet_activity_raid_status"},
	{Schema: "structs", Table: "struct_attribute", Trigger: "planet_activity_struct_attribute"},
	{Schema: "structs", Table: "player", Trigger: "update_address_guild_id"},
	{Schema: "structs", Table: "infusion", Trigger: "add_infusion_ledger_entry"},
	{Schema: "structs", Table: "planet", Trigger: "name_planet"},
	{Schema: "cache", Table: "blocks", Trigger: "transfer_ledger_entry"},
	{Schema: "cache", Table: "blocks", Trigger: "add_queue"},
}

// probeDroppedTriggers asserts that none of the cache-era triggers are
// still enabled. After the Phase B SQL runs, all of them should be
// absent; until it runs, sync-state still produces correct output but
// the doctor warns operators to apply the cutover.
func probeDroppedTriggers(ctx context.Context, pool *pgxpool.Pool) *Check {
	var stillOn []string
	for _, t := range droppedTriggers {
		var enabled string
		err := pool.QueryRow(ctx, `
			SELECT t.tgenabled::text
			  FROM pg_trigger t
			  JOIN pg_class  c ON c.oid = t.tgrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1
			   AND c.relname = $2
			   AND t.tgname ILIKE $3
			   AND NOT t.tgisinternal
			 LIMIT 1
		`, t.Schema, t.Table, t.Trigger).Scan(&enabled)
		if err != nil {
			continue // trigger absent → good
		}
		if enabled != "D" {
			stillOn = append(stillOn, fmt.Sprintf("%s.%s.%s", t.Schema, t.Table, t.Trigger))
		}
	}
	if len(stillOn) == 0 {
		return &Check{
			Name:     "cache-era triggers",
			Severity: OK,
			Detail:   "all dropped/disabled (sync-state owns derivations)",
		}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d cache-era trigger(s) still enabled; sync-state would double-write:", len(stillOn)))
	for _, name := range stillOn {
		b.WriteString("\n      " + name + " — apply the cache-retirement SQL or ALTER TRIGGER ... DISABLE")
	}
	return &Check{
		Name:     "cache-era triggers",
		Severity: FATAL,
		Detail:   b.String(),
	}
}

// canonicalColumns is the list of (schema, table, column) triples that the
// Phase B SQL (retire-cache.sql step 1) adds natively. Earlier sync-state
// releases bootstrapped these via runtime ALTER ADD COLUMN IF NOT EXISTS;
// once Phase B is the source of truth we assert their presence at startup
// and refuse to ingest against a DB that hasn't applied the migration.
//
// Each table is gated on the table existing in the first place — a fresh
// DB that hasn't deployed structs-pg at all is allowed to start because
// sync-state can't write to nonexistent tables either way.
var canonicalColumns = []struct{ Schema, Table, Column string }{
	{"structs", "current_block", "status"},
	{"structs", "current_block", "lag_blocks"},
	{"structs", "current_block", "tip_height"},
	{"structs", "planet_activity", "block_height"},
	{"structs", "stat_ore", "block_height"},
	{"structs", "stat_fuel", "block_height"},
	{"structs", "stat_capacity", "block_height"},
	{"structs", "stat_load", "block_height"},
	{"structs", "stat_structs_load", "block_height"},
	{"structs", "stat_power", "block_height"},
	{"structs", "stat_connection_capacity", "block_height"},
	{"structs", "stat_connection_count", "block_height"},
	{"structs", "stat_struct_health", "block_height"},
	{"structs", "stat_struct_status", "block_height"},
}

// probeCanonicalSchema asserts that every column in canonicalColumns
// either exists OR its table doesn't exist (fresh DB). Anything else
// is FATAL — the operator has applied a newer sync-state but not the
// matching retire-cache.sql, and sync-state's INSERTs will fail on the
// first commit.
func probeCanonicalSchema(ctx context.Context, pool *pgxpool.Pool) *Check {
	var missing []string
	for _, c := range canonicalColumns {
		var tableExists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = $2)`,
			c.Schema, c.Table,
		).Scan(&tableExists); err != nil || !tableExists {
			continue
		}
		var colExists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				 WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
			)`,
			c.Schema, c.Table, c.Column,
		).Scan(&colExists); err != nil {
			missing = append(missing, fmt.Sprintf("%s.%s.%s (introspect err: %v)", c.Schema, c.Table, c.Column, err))
			continue
		}
		if !colExists {
			missing = append(missing, fmt.Sprintf("%s.%s.%s", c.Schema, c.Table, c.Column))
		}
	}
	if len(missing) == 0 {
		return &Check{
			Name:     "canonical schema",
			Severity: OK,
			Detail:   "all canonical columns present (retire-cache.sql step 1 applied or table absent)",
		}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d canonical column(s) missing — apply retire-cache.sql step 1 before starting:", len(missing)))
	for _, m := range missing {
		b.WriteString("\n      " + m)
	}
	return &Check{
		Name:     "canonical schema",
		Severity: FATAL,
		Detail:   b.String(),
	}
}

// probeConcurrentUpdateCache WARNs if cache.attributes_tmp was written
// recently (within ~30s), suggesting update-cache may be running.
func probeConcurrentUpdateCache(ctx context.Context, pool *pgxpool.Pool) *Check {
	var newest *time.Time
	err := pool.QueryRow(ctx, `
		SELECT MAX(created_at) FROM cache.attributes_tmp
	`).Scan(&newest)
	if err != nil {
		// cache.attributes_tmp doesn't exist (fresh DB) -> nothing to worry about
		return nil
	}
	if newest == nil {
		return nil
	}
	age := time.Since(*newest)
	if age > 30*time.Second {
		return nil
	}
	return &Check{
		Name:     "update-cache concurrency",
		Severity: WARN,
		Detail:   fmt.Sprintf("cache.attributes_tmp written %s ago; update-cache may be running concurrently (forbidden in prod)", age.Round(time.Second)),
	}
}
