package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"sync-state/internal/db"
	"sync-state/internal/events"
	"sync-state/internal/rpc"
)

// Syncer owns the ingest loop: refresh tip, fetch window in parallel, commit
// blocks serially in ascending order, then sleep (or exit on one-shot).
type Syncer struct {
	cfg    Config
	client *rpc.Client
	pool   *db.Pool
	router *events.Router
	logger *log.Logger

	chainID string

	// tipWake, when non-nil, is the WebSocket `tm.event='NewBlock'`
	// push channel. The tip-idle loop selects on it so we react to a
	// new block within ~milliseconds instead of waiting for the next
	// poll tick. nil = no push notifier (falls back to pure polling,
	// e.g. tests, or when the node doesn't expose /websocket).
	tipWake <-chan int64
}

// NewSyncer wires the dependencies. chainID has already been validated by
// the doctor; pool already holds the writer advisory lock.
func NewSyncer(cfg Config, client *rpc.Client, pool *db.Pool, router *events.Router, chainID string) *Syncer {
	return &Syncer{
		cfg:     cfg,
		client:  client,
		pool:    pool,
		router:  router,
		chainID: chainID,
		logger:  log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds),
	}
}

// WithTipNotifier attaches a WebSocket push channel for new-block events.
// When set, the syncer's tip-idle wait selects on this channel and the
// poll interval becomes a *failsafe* (covers WebSocket gaps + the dial
// loop's startup time) rather than the primary signal.
func (s *Syncer) WithTipNotifier(ch <-chan int64) *Syncer {
	s.tipWake = ch
	return s
}

// fetchResult carries one prefetched bundle.
type fetchResult struct {
	height int64
	bundle *BlockBundle
	err    error
}

// fetchedWindow is one fully-fetched contiguous window of blocks, ready to
// hand to the applier. Used by the Run loop's prefetch pipeline so the
// fetch of window N+1 overlaps the apply of window N (matters in bulk mode
// where apply is a single ~800ms transaction).
type fetchedWindow struct {
	start, end int64
	tip        int64
	useBulk    bool
	bundles    []*BlockBundle
	err        error
}

// Run starts ingesting from `start`. Blocks until ctx is cancelled or
// stopHeight/one-shot conditions are met.
//
// When -one-shot is NOT set (the default), the loop polls /status every
// PollInterval and tracks the node's tip as it advances — including the
// case where the node is itself catching up to the chain tip. sync-state
// processes whatever blocks the node has made available, sleeps when
// there's nothing new, and resumes automatically. No restart needed.
//
// Bulk pipeline: while window N is committing (~800ms in bulk mode), the
// fetcher for window N+1 runs in parallel via a 1-deep prefetch buffer
// (`prefetched` channel). This recovers the wall-clock throughput lost
// when bulk mode replaced "stream apply during fetch" with "fetch all,
// then apply". Streaming mode skips prefetch (its inner select already
// overlaps fetch + apply per block).
func (s *Syncer) Run(ctx context.Context, start int64) error {
	next := start
	var lastTipLog time.Time
	// At most one pre-fetched window in flight. nil = nothing prefetched.
	// We never queue more than one because (a) it bounds memory and (b)
	// tip/useBulk decisions are re-made each iteration on fresh /status.
	var prefetched <-chan fetchedWindow

	// drainPrefetch cancels and waits for the prefetch goroutine to exit
	// when we need to discard it (tip recompute, ctx cancel, fatal err).
	prefetchCancel := func() {}
	defer func() { prefetchCancel() }()

	// Exponential backoff state for recovering from transient infra
	// failures (PG / RPC bounce, ephemeral network). Reset after every
	// successful fetch+apply cycle. See retry.go for the classifier and
	// the design notes — net is: on a transient err we mark the cursor
	// stalled, sleep with backoff, re-read the cursor (so we resume from
	// whatever last committed, which may include partial progress from a
	// streaming-mode window), and re-enter the top of this loop. We
	// never give up — operators expect ingest to self-heal across
	// Postgres restarts without process restart.
	var transient transientBackoff
	// recoverFromTransient is the common tail used by both the fetch and
	// apply error paths. Returns false if ctx was cancelled mid-sleep
	// (caller should return ctx.Err()), true to continue the loop.
	recoverFromTransient := func(label string, err error) bool {
		s.logger.Printf("WARN: %s transient infra error: %v (mark stalled, retry after backoff)", label, err)
		s.markStalled(ctx)
		if prefetched != nil {
			prefetchCancel()
			<-prefetched
			prefetched = nil
			prefetchCancel = func() {}
		}
		d := transient.Next()
		if !sleepCtx(ctx, d) {
			return false
		}
		// Re-read the cursor: some blocks in the failed window may have
		// committed before the failure (streaming mode commits per
		// block). Resume from cursor + 1; if the cursor is stale or
		// unreadable, keep `next` as-is and let the top of the loop
		// detect the situation via /status.
		if c, cerr := db.ReadCursor(ctx, s.pool.Pool, s.chainID); cerr == nil && c.LastHeight > 0 && c.LastHeight+1 > next {
			next = c.LastHeight + 1
		}
		return true
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		status, err := s.client.Status(ctx)
		if err != nil {
			s.logger.Printf("status failed: %v (sleeping %s)", err, s.cfg.PollInterval)
			s.markStalled(ctx)
			if !sleepCtx(ctx, s.cfg.PollInterval) {
				return ctx.Err()
			}
			continue
		}
		tip := status.Latest()
		if s.chainID != status.NodeInfo.Network {
			return fmt.Errorf("chain id changed mid-run: was %s, now %s", s.chainID, status.NodeInfo.Network)
		}

		end := tip
		if s.cfg.StopHeight > 0 && s.cfg.StopHeight < end {
			end = s.cfg.StopHeight
		}

		if next > end {
			// Tip-idle. Any prefetched bundle is stale (its tip and useBulk
			// decision are from before this idle period); discard so the
			// next active window re-decides cleanly.
			if prefetched != nil {
				prefetchCancel()
				<-prefetched
				prefetched = nil
				prefetchCancel = func() {}
			}
			if s.cfg.OneShot || (s.cfg.StopHeight > 0 && next > s.cfg.StopHeight) {
				return nil
			}
			s.updateCursorTip(ctx, next-1, tip)
			if time.Since(lastTipLog) >= 30*time.Second {
				switch {
				case status.SyncInfo.CatchingUp:
					s.logger.Printf("at node-tip h=%d (node still catching_up; will track as it advances)", next-1)
				case s.tipWake != nil:
					s.logger.Printf("at chain-tip h=%d (push-notified, poll %s failsafe)", next-1, s.cfg.PollInterval)
				default:
					s.logger.Printf("at chain-tip h=%d (sleeping %s between polls)", next-1, s.cfg.PollInterval)
				}
				lastTipLog = time.Now()
			}
			if !s.waitForTipChange(ctx) {
				return ctx.Err()
			}
			continue
		}
		lastTipLog = time.Time{}

		lag := end - next + 1
		useBulk := s.cfg.BulkEnabled && lag >= int64(s.cfg.BulkLagThreshold)
		windowSize := s.effectiveWindowSize(useBulk)
		windowEnd := next + int64(windowSize) - 1
		if windowEnd > end {
			windowEnd = end
		}

		// Fetch this window: either consume the prefetch from the previous
		// iteration, or fetch synchronously if we don't have one yet (first
		// iteration, or just transitioned from streaming → bulk, or the
		// prefetch's tip is stale).
		var fw fetchedWindow
		if prefetched != nil {
			fw = <-prefetched
			prefetched = nil
			prefetchCancel = func() {}
			// If the prefetched window doesn't match what we'd plan now
			// (tip advanced, lag class changed, or end re-clamped), the
			// fetch was wasted; discard and re-fetch synchronously. Cheap
			// because we only ever pre-fetch one window.
			if fw.start != next || fw.end != windowEnd || fw.useBulk != useBulk {
				fw = fetchedWindow{}
			}
		}
		if fw.bundles == nil && fw.err == nil {
			fw = s.fetchWindow(ctx, next, windowEnd, tip, useBulk)
		}
		if fw.err != nil {
			label := fmt.Sprintf("fetch [%d..%d]", next, windowEnd)
			if isTransientInfraErr(fw.err) {
				if !recoverFromTransient(label, fw.err) {
					return ctx.Err()
				}
				continue
			}
			return fmt.Errorf("%s: %w", label, fw.err)
		}

		// Start prefetching the NEXT window now (before we apply the
		// current one). Only in bulk mode — streaming already overlaps
		// fetch + apply per block. Skip when we're on the last window.
		if useBulk && windowEnd < end {
			nextStart := windowEnd + 1
			nextEnd := nextStart + int64(s.effectiveWindowSize(true)) - 1
			if nextEnd > end {
				nextEnd = end
			}
			pCtx, pCancel := context.WithCancel(ctx)
			ch := make(chan fetchedWindow, 1)
			go func() {
				ch <- s.fetchWindow(pCtx, nextStart, nextEnd, tip, true)
			}()
			prefetched = ch
			prefetchCancel = pCancel
		}

		if err := s.applyFetched(ctx, fw); err != nil {
			label := fmt.Sprintf("apply [%d..%d]", fw.start, fw.end)
			if isTransientInfraErr(err) {
				if !recoverFromTransient(label, err) {
					return ctx.Err()
				}
				continue
			}
			// Discard any prefetch before returning so the goroutine doesn't
			// leak when the caller decides to abort.
			if prefetched != nil {
				prefetchCancel()
				<-prefetched
			}
			return err
		}
		// Successful fetch+apply: clear any accumulated backoff so the
		// next blip starts fresh at 1s.
		transient.Reset()
		next = windowEnd + 1
	}
}

// effectiveWindowSize returns how many blocks one fetch/apply cycle covers.
// In bulk mode the window is capped at BulkWindow.
func (s *Syncer) effectiveWindowSize(useBulk bool) int {
	if !useBulk {
		return s.cfg.BatchSize
	}
	if s.cfg.BulkWindow <= 0 {
		return s.cfg.BatchSize
	}
	if s.cfg.BulkWindow < s.cfg.BatchSize {
		return s.cfg.BulkWindow
	}
	return s.cfg.BulkWindow
}

// fetchWindow fetches blocks [start..end] in parallel (Parallelism workers),
// reorders the results into ascending order, and returns one fetchedWindow.
// In streaming mode it also applies each block as it lands — keeping the
// pre-bulk semantics where the fetch + apply pipeline naturally overlaps.
//
// In bulk mode the function only fetches; the caller invokes applyFetched
// which runs the whole window through applyBulkWindow in one outer tx.
// Splitting fetch/apply this way is what lets Run() prefetch window N+1
// while window N's bulk commit is in flight.
func (s *Syncer) fetchWindow(ctx context.Context, start, end, tip int64, useBulk bool) fetchedWindow {
	fw := fetchedWindow{start: start, end: end, tip: tip, useBulk: useBulk}

	wCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, s.cfg.Parallelism)
	results := make(chan fetchResult, s.cfg.Parallelism*2)

	go func() {
		for h := start; h <= end; h++ {
			select {
			case sem <- struct{}{}:
			case <-wCtx.Done():
				return
			}
			h := h
			go func() {
				defer func() { <-sem }()
				bundle, err := s.fetchBundle(wCtx, h)
				select {
				case results <- fetchResult{height: h, bundle: bundle, err: err}:
				case <-wCtx.Done():
				}
			}()
		}
	}()

	pending := make(map[int64]*BlockBundle)
	expected := start
	startedAt := time.Now()
	processedInWindow := int64(0)
	var streamBundles []*BlockBundle
	var bulkBundles []*BlockBundle
	if useBulk {
		bulkBundles = make([]*BlockBundle, 0, end-start+1)
	} else {
		streamBundles = make([]*BlockBundle, 0, end-start+1)
	}

	for expected <= end {
		select {
		case <-wCtx.Done():
			fw.err = wCtx.Err()
			return fw
		case r := <-results:
			if r.err != nil {
				fw.err = fmt.Errorf("fetch h=%d: %w", r.height, r.err)
				return fw
			}
			pending[r.height] = r.bundle
			for {
				bundle, ok := pending[expected]
				if !ok {
					break
				}
				delete(pending, expected)
				if useBulk {
					bulkBundles = append(bulkBundles, bundle)
				} else {
					// Streaming: apply immediately so the apply
					// pipeline overlaps with in-flight fetches.
					if err := s.applyBlock(ctx, bundle, tip); err != nil {
						fw.err = fmt.Errorf("apply h=%d: %w", expected, err)
						return fw
					}
					streamBundles = append(streamBundles, bundle)
				}
				processedInWindow++
				if s.cfg.LogEvery > 0 && expected%int64(s.cfg.LogEvery) == 0 {
					rate := float64(processedInWindow) / time.Since(startedAt).Seconds()
					s.logUnknownSummary()
					mode := "stream"
					if useBulk {
						mode = "bulk"
					}
					s.logger.Printf("fetched h=%d (window %d-%d, %s, %.1f blocks/s, lag=%d)",
						expected, start, end, mode, rate, tip-expected)
				}
				expected++
			}
		}
	}

	if useBulk {
		fw.bundles = bulkBundles
	} else {
		fw.bundles = streamBundles
	}
	return fw
}

// applyFetched commits a previously-fetched window. In bulk mode this is
// one applyBulkWindow call (the per-window tx). In streaming mode the
// per-block applies already happened inside fetchWindow; this just logs
// the window summary and flushes the unknown-key deltas.
func (s *Syncer) applyFetched(ctx context.Context, fw fetchedWindow) error {
	startedAt := time.Now()
	if fw.useBulk {
		if err := s.applyBulkWindow(ctx, fw.bundles, fw.tip); err != nil {
			return fmt.Errorf("bulk apply [%d..%d]: %w", fw.start, fw.end, err)
		}
	}
	dur := time.Since(startedAt)
	modeLabel := "stream"
	if fw.useBulk {
		modeLabel = "bulk"
	}
	n := fw.end - fw.start + 1
	s.logger.Printf("window done (%s): %d blocks [%d..%d] in %s (%.1f blocks/s)",
		modeLabel, n, fw.start, fw.end, dur.Round(time.Millisecond),
		float64(n)/dur.Seconds())

	// Persist any unknown composite_keys we accumulated this window so
	// operators can query sync_state.unknown_event_log to see the full
	// coverage gap. Best-effort: a flush failure doesn't roll back
	// ingest (the next window will retry with the additional deltas).
	s.flushUnknowns(ctx)
	return nil
}

// flushUnknowns drains the router's accumulated unknown-composite-key
// deltas and UPSERTs them into sync_state.unknown_event_log. Called at
// the end of every window so the table stays close to real-time and an
// operator can `SELECT * FROM sync_state.unknown_event_log ORDER BY
// count DESC` at any time during the run.
func (s *Syncer) flushUnknowns(ctx context.Context) {
	deltas := s.router.DrainUnknownDeltas()
	if len(deltas) == 0 {
		return
	}
	entries := make(map[string]db.UnknownDelta, len(deltas))
	for k, v := range deltas {
		entries[k] = db.UnknownDelta{
			Count:         v.Count,
			FirstHeight:   v.FirstHeight,
			LastHeight:    v.LastHeight,
			SamplePayload: v.SamplePayload,
		}
	}
	if err := db.UpsertUnknownEntries(ctx, s.pool.Pool, s.chainID, entries); err != nil {
		s.logger.Printf("WARN: flush unknown_event_log (%d keys): %v", len(entries), err)
	}
}

func (s *Syncer) fetchBundle(ctx context.Context, height int64) (*BlockBundle, error) {
	type blockOut struct {
		r   *rpc.BlockResult
		err error
	}
	type resultsOut struct {
		r   *rpc.BlockResultsResult
		err error
	}
	bCh := make(chan blockOut, 1)
	rCh := make(chan resultsOut, 1)

	go func() {
		r, err := s.client.Block(ctx, height)
		bCh <- blockOut{r, err}
	}()
	go func() {
		r, err := s.client.BlockResults(ctx, height)
		rCh <- resultsOut{r, err}
	}()

	bo := <-bCh
	ro := <-rCh
	if bo.err != nil {
		return nil, fmt.Errorf("/block: %w", bo.err)
	}
	if ro.err != nil {
		return nil, fmt.Errorf("/block_results: %w", ro.err)
	}
	if err := validateBundle(bo.r, ro.r, s.chainID, height); err != nil {
		return nil, err
	}

	return &BlockBundle{
		Height:              height,
		ChainID:             bo.r.Block.Header.ChainID,
		BlockTime:           bo.r.Block.Header.Time,
		BlockHashHex:        bo.r.BlockID.Hash,
		ProposerHex:         bo.r.Block.Header.ProposerAddress,
		RawTxs:              bo.r.Block.Data.Txs,
		TxResults:           ro.r.TxsResults,
		FinalizeBlockEvents: ro.r.FinalizeBlockEvents,
	}, nil
}

// updateCursorTip updates the cursor status/lag/tip without committing a new
// block (e.g. when we're at the tip and just polling). Best-effort; logs and
// returns on error.
func (s *Syncer) updateCursorTip(ctx context.Context, height, tip int64) {
	status := db.ComputeStatus(height, tip)
	lag := tip - height
	if lag < 0 {
		lag = 0
	}
	if err := db.SetStatusOnly(ctx, s.pool.Pool, s.chainID, status, lag, tip); err != nil {
		// If the cursor row doesn't exist yet (no blocks committed),
		// this is a no-op UPDATE which doesn't error.
		s.logger.Printf("WARN: set cursor status: %v", err)
	}
}

func (s *Syncer) markStalled(ctx context.Context) {
	// Read current last_height to keep the row consistent.
	c, err := db.ReadCursor(ctx, s.pool.Pool, s.chainID)
	if err != nil {
		return
	}
	if c.LastHeight == 0 {
		return
	}
	_ = db.SetStatusOnly(ctx, s.pool.Pool, s.chainID, db.StatusStalled, c.LagBlocks, c.TipHeight)
}

func (s *Syncer) logUnknownSummary() {
	total, byKey := s.router.UnknownSummary()
	if total == 0 {
		return
	}
	s.logger.Printf("unknown composite_keys seen: total=%d distinct=%d (top hits will be logged on next interval)",
		total, len(byKey))
	// log top 5 (small enough to dump completely if <=5)
	const N = 5
	type kv struct {
		k string
		v uint64
	}
	pairs := make([]kv, 0, len(byKey))
	for k, v := range byKey {
		pairs = append(pairs, kv{k, v})
	}
	// simple in-place selection of top N
	for i := 0; i < N && i < len(pairs); i++ {
		maxIdx := i
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].v > pairs[maxIdx].v {
				maxIdx = j
			}
		}
		pairs[i], pairs[maxIdx] = pairs[maxIdx], pairs[i]
		s.logger.Printf("  unknown[%d] %s = %d", i+1, pairs[i].k, pairs[i].v)
	}
	s.router.ResetUnknownSummary()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// waitForTipChange blocks until something suggests a new block might be
// available: either the WebSocket notifier pushed a NewBlock event, or
// the poll interval elapsed (failsafe). Returns false on ctx cancel.
//
// Drains the notifier channel before returning to coalesce bursts (the
// chain may emit multiple NewBlock frames while we're already polling
// /status; we don't need a wake per block).
func (s *Syncer) waitForTipChange(ctx context.Context) bool {
	if s.tipWake == nil {
		return sleepCtx(ctx, s.cfg.PollInterval)
	}
	t := time.NewTimer(s.cfg.PollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.tipWake:
		// Drain anything else that piled up while we were idle so
		// the next loop iteration doesn't immediately re-wake.
		for {
			select {
			case <-s.tipWake:
			default:
				return true
			}
		}
	case <-t.C:
		return true
	}
}

// ResolveStart resolves the actual starting height: override > resume cursor
// > earliest. Clamps to node's earliest if cursor would resume below it.
func ResolveStart(ctx context.Context, pool *db.Pool, chainID string, override int64, earliest int64) (int64, error) {
	if override > 0 {
		return override, nil
	}
	c, err := db.ReadCursor(ctx, pool.Pool, chainID)
	if err != nil {
		return 0, err
	}
	if c.LastHeight > 0 {
		next := c.LastHeight + 1
		if next < earliest {
			return earliest, nil
		}
		return next, nil
	}
	if earliest > 0 {
		return earliest, nil
	}
	return 1, nil
}

// CheckReorg compares the last committed block_hash to what the RPC node
// currently returns for that height. Mismatch = fatal (operator likely
// switched nodes or the node was restored from a stale snapshot).
func CheckReorg(ctx context.Context, client *rpc.Client, pool *db.Pool, chainID string) error {
	c, err := db.ReadCursor(ctx, pool.Pool, chainID)
	if err != nil {
		return err
	}
	if c.LastHeight == 0 || c.LastBlockHash == "" {
		return nil
	}
	got, err := client.Block(ctx, c.LastHeight)
	if err != nil {
		return fmt.Errorf("reorg check fetch h=%d: %w", c.LastHeight, err)
	}
	if got.BlockID.Hash != c.LastBlockHash {
		return fmt.Errorf("reorg detected: cursor last_height=%d hash=%s but node now returns hash=%s",
			c.LastHeight, c.LastBlockHash, got.BlockID.Hash)
	}
	return nil
}

// IsContextErr reports whether err is cancellation or a wrapped cancellation.
func IsContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// unused: keep go vet happy when fields are not referenced yet
var _ = strconv.Atoi
