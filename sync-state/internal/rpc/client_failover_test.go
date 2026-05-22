package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNode is an httptest harness that pretends to be a CometBFT RPC.
// Per-route handlers can be swapped at runtime to simulate flapping,
// catching-up, chain_id mismatch, etc. — every test owns its own pair.
type fakeNode struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	chainID string
	tip     int64
	earlies int64
	catchUp bool
	// per-path overrides; nil means "use default handler"
	statusHandler       http.HandlerFunc
	blockHandler        http.HandlerFunc
	blockResultsHandler http.HandlerFunc

	statusHits       int64
	blockHits        int64
	blockResultsHits int64
}

func newFakeNode(t *testing.T, chainID string, tip int64) *fakeNode {
	t.Helper()
	fn := &fakeNode{
		t:       t,
		chainID: chainID,
		tip:     tip,
		earlies: 1,
	}
	fn.server = httptest.NewServer(http.HandlerFunc(fn.route))
	t.Cleanup(func() { fn.server.Close() })
	return fn
}

func (f *fakeNode) URL() string { return f.server.URL }

func (f *fakeNode) setTip(tip int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tip = tip
}

func (f *fakeNode) setCatchUp(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catchUp = b
}

func (f *fakeNode) setChainID(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chainID = id
}

func (f *fakeNode) setStatusHandler(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusHandler = h
}

func (f *fakeNode) setBlockHandler(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockHandler = h
}

func (f *fakeNode) setBlockResultsHandler(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockResultsHandler = h
}

func (f *fakeNode) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/status"):
		atomic.AddInt64(&f.statusHits, 1)
		f.mu.Lock()
		h := f.statusHandler
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		f.defaultStatus(w, r)
	case strings.HasPrefix(r.URL.Path, "/block_results"):
		atomic.AddInt64(&f.blockResultsHits, 1)
		f.mu.Lock()
		h := f.blockResultsHandler
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		f.defaultBlockResults(w, r)
	case strings.HasPrefix(r.URL.Path, "/block"):
		atomic.AddInt64(&f.blockHits, 1)
		f.mu.Lock()
		h := f.blockHandler
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		f.defaultBlock(w, r)
	case strings.HasPrefix(r.URL.Path, "/tx_search"):
		writeOK(w, json.RawMessage(`{"txs":[],"total_count":"0"}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeNode) defaultStatus(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload := fmt.Sprintf(`{
		"node_info":{"network":%q},
		"sync_info":{
			"latest_block_height":%q,
			"earliest_block_height":%q,
			"latest_block_time":"2026-05-20T00:00:00Z",
			"catching_up":%v
		}
	}`, f.chainID, strconv.FormatInt(f.tip, 10), strconv.FormatInt(f.earlies, 10), f.catchUp)
	writeOK(w, json.RawMessage(payload))
}

func (f *fakeNode) defaultBlock(w http.ResponseWriter, r *http.Request) {
	h, _ := strconv.ParseInt(r.URL.Query().Get("height"), 10, 64)
	f.mu.Lock()
	tip := f.tip
	chainID := f.chainID
	f.mu.Unlock()
	if h > tip {
		writeRPCError(w, -32603, fmt.Sprintf("height %d must be less than or equal to the current blockchain height %d", h, tip))
		return
	}
	payload := fmt.Sprintf(`{
		"block_id":{"hash":"AAAA"},
		"block":{
			"header":{"height":%q,"chain_id":%q,"time":"2026-05-20T00:00:00Z","proposer_address":"BBBB"},
			"data":{"txs":[]}
		}
	}`, strconv.FormatInt(h, 10), chainID)
	writeOK(w, json.RawMessage(payload))
}

func (f *fakeNode) defaultBlockResults(w http.ResponseWriter, r *http.Request) {
	h, _ := strconv.ParseInt(r.URL.Query().Get("height"), 10, 64)
	f.mu.Lock()
	tip := f.tip
	f.mu.Unlock()
	if h > tip {
		writeRPCError(w, -32603, "failed to load block")
		return
	}
	payload := fmt.Sprintf(`{"height":%q,"txs_results":[],"finalize_block_events":[]}`,
		strconv.FormatInt(h, 10))
	writeOK(w, json.RawMessage(payload))
}

func writeOK(w http.ResponseWriter, result json.RawMessage) {
	env := Envelope{JSONRPC: "2.0", ID: json.RawMessage(`-1`), Result: result}
	_ = json.NewEncoder(w).Encode(env)
}

func writeRPCError(w http.ResponseWriter, code int, msg string) {
	env := Envelope{JSONRPC: "2.0", ID: json.RawMessage(`-1`), Error: &RPCError{Code: code, Message: msg}}
	_ = json.NewEncoder(w).Encode(env)
}

// ---------------------------------------------------------------------------
// Failover behaviour
// ---------------------------------------------------------------------------

func TestClient_PrimaryHealthy_NeverHitsSeed(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 1000)
	seed := newFakeNode(t, "structstestnet-111", 1000)

	c := NewClient([]string{primary.URL(), seed.URL()}, 5*time.Second, 0)
	if _, err := c.Block(context.Background(), 500); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if atomic.LoadInt64(&primary.blockHits) != 1 {
		t.Fatalf("primary should have served the request once, got %d", primary.blockHits)
	}
	if atomic.LoadInt64(&seed.blockHits) != 0 {
		t.Fatalf("seed should not have been hit when primary is healthy")
	}
}

func TestClient_PrimaryHTTP500_FallsToSeed(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 1000)
	seed := newFakeNode(t, "structstestnet-111", 1000)
	primary.setBlockHandler(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)
	if _, err := c.Block(context.Background(), 500); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if atomic.LoadInt64(&seed.blockHits) == 0 {
		t.Fatalf("seed should have served the request after primary 500")
	}
}

func TestClient_PrimaryHeightNotAvailable_FallsToSeed(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 100) // behind
	seed := newFakeNode(t, "structstestnet-111", 1000)

	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)
	if _, err := c.Block(context.Background(), 500); err != nil {
		t.Fatalf("Block: %v", err)
	}
	// First request: primary has no /status cache yet, so it must
	// actually be tried and reject the block before we fall through.
	if atomic.LoadInt64(&primary.blockHits) != 1 {
		t.Fatalf("primary should have been tried once, got %d", primary.blockHits)
	}
	if atomic.LoadInt64(&seed.blockHits) != 1 {
		t.Fatalf("seed should have served the request after primary said 'height not available'")
	}
}

func TestClient_PrimaryCatchingUp_StatusCacheSkipsDoomedRoundTrip(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 100)
	primary.setCatchUp(true)
	seed := newFakeNode(t, "structstestnet-111", 1000)

	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)

	// Warm the status cache so the selection algorithm knows primary's tip.
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if atomic.LoadInt64(&primary.statusHits) != 1 {
		t.Fatalf("status should have hit primary once")
	}

	// Now request a block above primary's tip; primary should be
	// skipped entirely (no /block hit) and seed should serve it.
	if _, err := c.Block(context.Background(), 500); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if got := atomic.LoadInt64(&primary.blockHits); got != 0 {
		t.Fatalf("primary should have been skipped via status cache, got %d hits", got)
	}
	if atomic.LoadInt64(&seed.blockHits) != 1 {
		t.Fatalf("seed should have served the block")
	}
}

func TestClient_PrimaryRecovers_PreferenceRestored(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 100)
	seed := newFakeNode(t, "structstestnet-111", 1000)

	// Short TTL so the cached "primary is behind" expires quickly.
	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)
	c.statusTTL = 50 * time.Millisecond

	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := c.Block(context.Background(), 500); err != nil {
		t.Fatalf("Block (during catchup): %v", err)
	}
	if got := atomic.LoadInt64(&seed.blockHits); got != 1 {
		t.Fatalf("seed should have served the first block, got %d", got)
	}

	primary.setTip(2000)
	// Force the cached status to expire and re-probe.
	time.Sleep(60 * time.Millisecond)
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status after recovery: %v", err)
	}
	if _, err := c.Block(context.Background(), 500); err != nil {
		t.Fatalf("Block after recovery: %v", err)
	}
	if got := atomic.LoadInt64(&primary.blockHits); got == 0 {
		t.Fatalf("primary should have served the post-recovery block, got 0 hits")
	}
}

func TestClient_AllEndpointsFail_ReturnsAggregateError(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 1000)
	seed := newFakeNode(t, "structstestnet-111", 1000)
	primary.setBlockHandler(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "p-down", http.StatusInternalServerError)
	})
	seed.setBlockHandler(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "s-down", http.StatusInternalServerError)
	})

	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)
	_, err := c.Block(context.Background(), 500)
	if err == nil {
		t.Fatalf("expected error when all endpoints fail")
	}
	if !errors.Is(err, ErrAllEndpointsFailed) {
		t.Fatalf("expected ErrAllEndpointsFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "p-down") || !strings.Contains(err.Error(), "s-down") {
		t.Fatalf("aggregate error should mention both endpoints, got %v", err)
	}
}

func TestClient_DeterministicNon404_FailsFast(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 1000)
	seed := newFakeNode(t, "structstestnet-111", 1000)
	primary.setBlockHandler(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})

	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)
	_, err := c.Block(context.Background(), 500)
	if err == nil {
		t.Fatalf("expected deterministic error from 400")
	}
	if !errors.Is(err, ErrRPCDeterministic) {
		t.Fatalf("expected ErrRPCDeterministic for 4xx, got %v", err)
	}
	if atomic.LoadInt64(&seed.blockHits) != 0 {
		t.Fatalf("seed should NOT be tried after deterministic 4xx from primary")
	}
}

func TestClient_SingleURL_NoFallbackAttempt(t *testing.T) {
	only := newFakeNode(t, "structstestnet-111", 1000)
	c := NewClient([]string{only.URL()}, 2*time.Second, 0)
	if _, err := c.Block(context.Background(), 500); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if got := atomic.LoadInt64(&only.blockHits); got != 1 {
		t.Fatalf("single endpoint should have served the request, got %d hits", got)
	}
}

func TestClient_DuplicateURLs_Deduplicated(t *testing.T) {
	only := newFakeNode(t, "structstestnet-111", 1000)
	c := NewClient([]string{only.URL(), only.URL(), ""}, 2*time.Second, 0)
	if got := len(c.URLs()); got != 1 {
		t.Fatalf("expected duplicate URLs to collapse to 1, got %d (%v)", got, c.URLs())
	}
}

func TestClient_ConcurrentRequests_NoRace(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 100)
	primary.setCatchUp(true)
	seed := newFakeNode(t, "structstestnet-111", 1000)

	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(h int64) {
			defer wg.Done()
			if _, err := c.Status(context.Background()); err != nil {
				t.Errorf("Status: %v", err)
			}
			if _, err := c.Block(context.Background(), h); err != nil {
				t.Errorf("Block(%d): %v", h, err)
			}
		}(int64(500 + i))
	}
	wg.Wait()
}

func TestClient_StatusOf_FetchesNamedEndpoint(t *testing.T) {
	primary := newFakeNode(t, "structstestnet-111", 100)
	seed := newFakeNode(t, "structstestnet-111", 1000)
	c := NewClient([]string{primary.URL(), seed.URL()}, 2*time.Second, 0)

	pStat, err := c.StatusOf(context.Background(), primary.URL())
	if err != nil {
		t.Fatalf("StatusOf primary: %v", err)
	}
	if got := pStat.Latest(); got != 100 {
		t.Fatalf("primary tip = %d, want 100", got)
	}
	sStat, err := c.StatusOf(context.Background(), seed.URL())
	if err != nil {
		t.Fatalf("StatusOf seed: %v", err)
	}
	if got := sStat.Latest(); got != 1000 {
		t.Fatalf("seed tip = %d, want 1000", got)
	}

	if _, err := c.StatusOf(context.Background(), "http://nope.example.com"); err == nil {
		t.Fatalf("expected error for unknown endpoint")
	}
}
