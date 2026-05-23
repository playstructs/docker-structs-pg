package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sync-state/internal/rpc"
)

// fakeRPC is a minimal CometBFT RPC stub shared across doctor pool
// tests. It supports per-test overrides of /status (chain_id mismatch,
// connection refused, etc.) and serves /block + /block_results well
// enough for the doctor's chain-wide probes to succeed.
type fakeRPC struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	chainID string
	tip     int64
	earlies int64
	catchUp bool
	dead    bool // simulates HTTP unreachable when true
}

func newFakeRPC(t *testing.T, chainID string, tip int64) *fakeRPC {
	t.Helper()
	fr := &fakeRPC{t: t, chainID: chainID, tip: tip, earlies: 1}
	fr.server = httptest.NewServer(http.HandlerFunc(fr.route))
	t.Cleanup(func() { fr.server.Close() })
	return fr
}

func (f *fakeRPC) url() string { return f.server.URL }

func (f *fakeRPC) kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead = true
}

func (f *fakeRPC) setChainID(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chainID = id
}

func (f *fakeRPC) setCatchUp(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catchUp = b
}

func (f *fakeRPC) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.dead {
		f.mu.Unlock()
		// Mimic "connection refused" by sending a 503 with no envelope.
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	chainID := f.chainID
	tip := f.tip
	earlies := f.earlies
	catchUp := f.catchUp
	f.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/status"):
		body := fmt.Sprintf(`{
			"node_info":{"network":%q},
			"sync_info":{
				"latest_block_height":%q,
				"earliest_block_height":%q,
				"latest_block_time":"2026-05-20T00:00:00Z",
				"catching_up":%v
			}
		}`, chainID, strconv.FormatInt(tip, 10), strconv.FormatInt(earlies, 10), catchUp)
		ok(w, body)
	case strings.HasPrefix(r.URL.Path, "/block_results"):
		h, _ := strconv.ParseInt(r.URL.Query().Get("height"), 10, 64)
		if h > tip {
			rpcErr(w, "failed to load block")
			return
		}
		ok(w, fmt.Sprintf(`{"height":%q,"txs_results":[{"code":0,"events":[]}],"finalize_block_events":[]}`, strconv.FormatInt(h, 10)))
	case strings.HasPrefix(r.URL.Path, "/block"):
		h, _ := strconv.ParseInt(r.URL.Query().Get("height"), 10, 64)
		if h > tip {
			rpcErr(w, fmt.Sprintf("height %d must be less than or equal to the current blockchain height %d", h, tip))
			return
		}
		ok(w, fmt.Sprintf(`{
			"block_id":{"hash":"AAAA"},
			"block":{
				"header":{"height":%q,"chain_id":%q,"time":"2026-05-20T00:00:00Z","proposer_address":"BBBB"},
				"data":{"txs":["YWFh"]}
			}
		}`, strconv.FormatInt(h, 10), chainID))
	case strings.HasPrefix(r.URL.Path, "/blockchain"):
		// Pretend there's a tx in the latest block so the abci retention
		// probe finds something to verify.
		ok(w, fmt.Sprintf(`{"last_height":%q,"block_metas":[{"num_txs":"1","header":{"height":%q}}]}`,
			strconv.FormatInt(tip, 10), strconv.FormatInt(tip, 10)))
	case strings.HasPrefix(r.URL.Path, "/tx_search"):
		ok(w, `{"txs":[{"hash":"DEAD","height":"1","index":0,"tx_result":{"code":0,"events":[]}}],"total_count":"1"}`)
	default:
		http.NotFound(w, r)
	}
}

func ok(w http.ResponseWriter, raw string) {
	_ = json.NewEncoder(w).Encode(rpc.Envelope{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`-1`),
		Result:  json.RawMessage(raw),
	})
}

func rpcErr(w http.ResponseWriter, msg string) {
	_ = json.NewEncoder(w).Encode(rpc.Envelope{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`-1`),
		Error:   &rpc.RPCError{Code: -32603, Message: msg},
	})
}

// ---------------------------------------------------------------------------

func TestDoctor_BothEndpointsHealthy_SameChain_VerdictOK(t *testing.T) {
	primary := newFakeRPC(t, "structstestnet-111", 1000)
	seed := newFakeRPC(t, "structstestnet-111", 1000)

	client := rpc.NewClient([]string{primary.url(), seed.url()}, 3*time.Second, 0)
	rep, err := Run(context.Background(), Inputs{
		RPC:                 client,
		Pool:                nil,
		ExpectedChainID:     "structstestnet-111",
		SkipCacheConcurrent: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Fatal {
		t.Fatalf("expected non-FATAL verdict, got %q. Checks: %+v", rep.Verdict, rep.Checks)
	}
	if len(rep.Endpoints) != 2 {
		t.Fatalf("expected 2 endpoint reports, got %d", len(rep.Endpoints))
	}
	if rep.Endpoints[0].Role != "primary" || rep.Endpoints[1].Role != "fallback" {
		t.Fatalf("endpoint roles wrong: %+v", rep.Endpoints)
	}
	for _, ep := range rep.Endpoints {
		if !ep.Reached {
			t.Fatalf("endpoint %s should be reached", ep.URL)
		}
		if ep.ChainID != "structstestnet-111" {
			t.Fatalf("endpoint %s chain_id = %q", ep.URL, ep.ChainID)
		}
	}
	if !containsCheck(rep.Checks, "rpc pool", OK) {
		t.Fatalf("expected OK rpc pool check; got %+v", rep.Checks)
	}
}

func TestDoctor_PrimaryDown_SeedHealthy_VerdictWARN(t *testing.T) {
	primary := newFakeRPC(t, "structstestnet-111", 1000)
	seed := newFakeRPC(t, "structstestnet-111", 1000)
	primary.kill()

	client := rpc.NewClient([]string{primary.url(), seed.url()}, 3*time.Second, 0)
	rep, err := Run(context.Background(), Inputs{
		RPC:                 client,
		Pool:                nil,
		ExpectedChainID:     "structstestnet-111",
		SkipCacheConcurrent: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Fatal {
		t.Fatalf("expected non-FATAL verdict (seed is healthy), got %q. Checks: %+v", rep.Verdict, rep.Checks)
	}
	if rep.Endpoints[0].Reached {
		t.Fatalf("primary endpoint should NOT be reached")
	}
	if !rep.Endpoints[1].Reached {
		t.Fatalf("seed endpoint should be reached")
	}
	if !containsCheck(rep.Checks, "rpc pool", WARN) {
		t.Fatalf("expected WARN rpc pool check (1 of 2 healthy); got %+v", rep.Checks)
	}
}

func TestDoctor_AllEndpointsDown_VerdictFATAL(t *testing.T) {
	primary := newFakeRPC(t, "structstestnet-111", 1000)
	seed := newFakeRPC(t, "structstestnet-111", 1000)
	primary.kill()
	seed.kill()

	client := rpc.NewClient([]string{primary.url(), seed.url()}, 1*time.Second, 0)
	rep, err := Run(context.Background(), Inputs{
		RPC:                 client,
		Pool:                nil,
		ExpectedChainID:     "structstestnet-111",
		SkipCacheConcurrent: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Fatal {
		t.Fatalf("expected FATAL verdict, got %q", rep.Verdict)
	}
	if !containsCheck(rep.Checks, "rpc pool", FATAL) {
		t.Fatalf("expected FATAL rpc pool check; got %+v", rep.Checks)
	}
}

func TestDoctor_ChainIDMismatchAcrossPool_VerdictFATAL(t *testing.T) {
	primary := newFakeRPC(t, "structstestnet-111", 1000)
	seed := newFakeRPC(t, "wrongchain-1", 1000)

	client := rpc.NewClient([]string{primary.url(), seed.url()}, 3*time.Second, 0)
	rep, err := Run(context.Background(), Inputs{
		RPC:                 client,
		Pool:                nil,
		ExpectedChainID:     "structstestnet-111",
		SkipCacheConcurrent: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Fatal {
		t.Fatalf("expected FATAL verdict for mismatched chain_id; got %q", rep.Verdict)
	}
	if !containsCheck(rep.Checks, "rpc pool", FATAL) {
		t.Fatalf("expected FATAL rpc pool check; got %+v", rep.Checks)
	}
	if !strings.Contains(rep.Verdict, "chain_id mismatch") {
		t.Fatalf("verdict should explain chain_id mismatch, got %q", rep.Verdict)
	}
}

func TestDoctor_ExpectedChainIDMismatch_VerdictFATAL(t *testing.T) {
	primary := newFakeRPC(t, "wrongchain-1", 1000)
	seed := newFakeRPC(t, "wrongchain-1", 1000)

	client := rpc.NewClient([]string{primary.url(), seed.url()}, 3*time.Second, 0)
	rep, err := Run(context.Background(), Inputs{
		RPC:                 client,
		Pool:                nil,
		ExpectedChainID:     "structstestnet-111",
		SkipCacheConcurrent: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Fatal {
		t.Fatalf("expected FATAL verdict for expected-chain-id mismatch; got %q", rep.Verdict)
	}
	if !containsCheck(rep.Checks, "chain_id", FATAL) {
		t.Fatalf("expected FATAL chain_id check; got %+v", rep.Checks)
	}
}

func TestDoctor_PrimaryCatchingUp_PerEndpointWARN_PoolStillOK(t *testing.T) {
	primary := newFakeRPC(t, "structstestnet-111", 100)
	primary.setCatchUp(true)
	seed := newFakeRPC(t, "structstestnet-111", 1000)

	client := rpc.NewClient([]string{primary.url(), seed.url()}, 3*time.Second, 0)
	rep, err := Run(context.Background(), Inputs{
		RPC:                 client,
		Pool:                nil,
		ExpectedChainID:     "structstestnet-111",
		SkipCacheConcurrent: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Fatal {
		t.Fatalf("catching_up should not be FATAL; got %q", rep.Verdict)
	}
	if !endpointHasCheck(rep.Endpoints[0], "node liveness", WARN) {
		t.Fatalf("primary should have WARN node liveness (catching_up); got %+v", rep.Endpoints[0].Checks)
	}
	if !endpointHasCheck(rep.Endpoints[1], "node liveness", OK) {
		t.Fatalf("seed should have OK node liveness; got %+v", rep.Endpoints[1].Checks)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func containsCheck(checks []Check, name string, sev Severity) bool {
	for _, c := range checks {
		if c.Name == name && c.Severity == sev {
			return true
		}
	}
	return false
}

func endpointHasCheck(ep EndpointReport, name string, sev Severity) bool {
	for _, c := range ep.Checks {
		if c.Name == name && c.Severity == sev {
			return true
		}
	}
	return false
}
