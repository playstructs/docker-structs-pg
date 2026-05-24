package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client is a CometBFT JSON-RPC client that fronts an ordered list of
// endpoints (primary first, seed last) and falls back transparently when
// the preferred endpoint errors, times out, or doesn't yet have the
// requested block (catching-up case).
//
// Trust model: every URL in the list is assumed to belong to the same
// chain and to be operator-trusted. Doctor enforces chain_id consistency
// at startup; the Client does not perform light-client validation.
type Client struct {
	endpoints  []*endpoint
	http       *http.Client
	maxRetries int
	backoff    time.Duration

	// statusTTL controls how long a per-endpoint /status snapshot is
	// considered fresh enough to influence selection. Default 5s keeps
	// us from skipping a primary that just caught up, while still
	// avoiding doomed round-trips during long catching_up windows.
	statusTTL time.Duration

	// failoverTimeout caps the time the failover loop spends on any
	// non-final endpoint before moving on. The LAST endpoint in the
	// pool (typically the seed) gets the full caller-supplied context
	// instead, so reliability at the tail is preserved. Default 8s —
	// long enough to dial + complete a single CometBFT RPC on a healthy
	// host, short enough that a hung primary doesn't stall a doctor
	// run for minutes.
	failoverTimeout time.Duration
}

// endpoint tracks one upstream RPC URL plus a cached /status snapshot so
// the selection algorithm can skip endpoints that are demonstrably unable
// to serve the requested block.
type endpoint struct {
	url string

	mu       sync.Mutex
	lastStat *StatusResult
	lastSeen time.Time
	lastErr  error
}

// NewClient builds a Client over an ordered URL list. The first non-empty
// URL is the preferred endpoint; subsequent URLs are fallbacks tried in
// order. Empty / duplicate URLs are filtered out so callers can safely
// pass `[primary, seed]` with primary unset.
func NewClient(urls []string, timeout time.Duration, maxRetries int) *Client {
	seen := make(map[string]struct{}, len(urls))
	eps := make([]*endpoint, 0, len(urls))
	for _, raw := range urls {
		u := strings.TrimRight(strings.TrimSpace(raw), "/")
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		eps = append(eps, &endpoint{url: u})
	}
	return &Client{
		endpoints: eps,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        128,
				MaxIdleConnsPerHost: 128,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		maxRetries:      maxRetries,
		backoff:         250 * time.Millisecond,
		statusTTL:       5 * time.Second,
		failoverTimeout: 8 * time.Second,
	}
}

// BaseURL returns the preferred endpoint URL for logging / display. When
// there are no endpoints (shouldn't happen in production), returns "".
func (c *Client) BaseURL() string {
	if len(c.endpoints) == 0 {
		return ""
	}
	return c.endpoints[0].url
}

// URLs returns every configured endpoint URL in preference order. Used by
// the doctor to probe each host.
func (c *Client) URLs() []string {
	out := make([]string, len(c.endpoints))
	for i, e := range c.endpoints {
		out[i] = e.url
	}
	return out
}

// ErrRPCDeterministic wraps non-retryable 4xx and CometBFT-level error
// envelopes — retrying won't make the data appear. Block-height-not-found
// is a *retryable* deterministic error: the same endpoint won't have it
// yet, but a different endpoint might, so we fall through to the next
// endpoint instead of returning immediately.
var ErrRPCDeterministic = errors.New("deterministic rpc error")

// ErrHeightNotAvailable signals "this endpoint doesn't have that block
// yet" (typically a catching-up primary). Treated as a soft failure that
// triggers fallback to the next endpoint without burning the retry
// budget on the same host.
var ErrHeightNotAvailable = errors.New("height not available at this endpoint")

// ErrAllEndpointsFailed is the final aggregate error when every
// configured endpoint refused to serve a request.
var ErrAllEndpointsFailed = errors.New("all rpc endpoints failed")

// get issues a GET against the preferred endpoint, falling back to the
// next endpoint on transient errors, http 5xx, and height-not-available
// rpc errors. wantHeight is used to skip endpoints whose cached /status
// reports a tip below the requested height (saves a doomed round-trip
// while the primary is still catching up). Pass 0 for requests that are
// not height-bound (/status, /tx_search).
func (c *Client) get(ctx context.Context, path string, wantHeight int64, out any) error {
	if len(c.endpoints) == 0 {
		return errors.New("rpc client has no endpoints configured")
	}

	var errs []string
	for i, ep := range c.endpoints {
		if c.skipForHeight(ep, wantHeight) {
			errs = append(errs, fmt.Sprintf("%s: skipped (cached tip < %d)", ep.url, wantHeight))
			continue
		}
		if c.skipForRecentFailure(ep, i) {
			errs = append(errs, fmt.Sprintf("%s: skipped (recent failure: %v)", ep.url, ep.cachedErr()))
			continue
		}
		epCtx, cancel := c.endpointContext(ctx, i)
		err := c.getFromEndpoint(epCtx, ep, path, out)
		cancel()
		switch {
		case err == nil:
			c.markHealthy(ep)
			return nil
		case errors.Is(err, ErrRPCDeterministic) && !errors.Is(err, ErrHeightNotAvailable):
			// Genuinely deterministic (rpc envelope error or 4xx that
			// isn't "block not found"). No other endpoint will serve
			// this either; fail fast.
			return err
		case ctx.Err() != nil:
			// Caller cancelled or hit their own deadline; surface that
			// rather than wrap it as ErrAllEndpointsFailed. (epCtx
			// hitting failoverTimeout is fine — fall through.)
			return ctx.Err()
		default:
			c.markFailed(ep, err)
			errs = append(errs, fmt.Sprintf("%s: %v", ep.url, err))
			continue
		}
	}
	return fmt.Errorf("%w: %s", ErrAllEndpointsFailed, strings.Join(errs, "; "))
}

// skipForRecentFailure returns true when this endpoint failed within
// the last statusTTL window AND it's not the last endpoint in the chain
// (we always try the final endpoint, otherwise a single bad blip would
// silently knock the whole pool out for statusTTL seconds). Skipping
// stops doctor / batch loops from re-discovering a dead primary on
// every request.
func (c *Client) skipForRecentFailure(ep *endpoint, i int) bool {
	if i == len(c.endpoints)-1 {
		return false
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.lastErr == nil {
		return false
	}
	return time.Since(ep.lastSeen) <= c.statusTTL
}

// markHealthy clears the cached error for an endpoint that just served
// a request successfully, restoring its preference in subsequent calls.
func (c *Client) markHealthy(ep *endpoint) {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.lastErr = nil
	ep.lastSeen = time.Now()
}

// markFailed caches an endpoint-level error so skipForRecentFailure can
// shortcut subsequent calls until the TTL expires.
func (c *Client) markFailed(ep *endpoint, err error) {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.lastErr = err
	ep.lastSeen = time.Now()
}

// cachedErr returns the endpoint's last recorded error under lock.
// Helper for log messages.
func (ep *endpoint) cachedErr() error {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.lastErr
}

// endpointContext returns a context for the i-th endpoint in the failover
// chain. Non-final endpoints get capped at failoverTimeout so a hung
// primary doesn't eat the full caller deadline; the final endpoint
// inherits the caller's ctx unchanged so the seed retains its full
// retry / timeout budget.
func (c *Client) endpointContext(parent context.Context, i int) (context.Context, context.CancelFunc) {
	if i == len(c.endpoints)-1 {
		return parent, func() {}
	}
	if c.failoverTimeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, c.failoverTimeout)
}

// skipForHeight returns true when wantHeight is known to exceed this
// endpoint's freshly-cached tip — i.e., the endpoint is catching up and
// definitely doesn't have the block yet. The TTL is short enough that a
// just-caught-up endpoint comes back into rotation quickly.
func (c *Client) skipForHeight(ep *endpoint, wantHeight int64) bool {
	if wantHeight <= 0 {
		return false
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.lastStat == nil {
		return false
	}
	if time.Since(ep.lastSeen) > c.statusTTL {
		return false
	}
	return ep.lastStat.Latest() < wantHeight
}

// getFromEndpoint runs the existing per-host retry loop against a single
// endpoint. Returns wrapped ErrHeightNotAvailable when the RPC envelope
// reports a missing block, so the outer loop can fall through cleanly.
func (c *Client) getFromEndpoint(ctx context.Context, ep *endpoint, path string, out any) error {
	full := ep.url + path
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			sleep := c.backoff * time.Duration(1<<uint(attempt-1))
			if sleep > 5*time.Second {
				sleep = 5 * time.Second
			}
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, full, truncate(string(body), 200))
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return fmt.Errorf("%w: %v", ErrRPCDeterministic, lastErr)
			}
			continue
		}
		var env Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			lastErr = fmt.Errorf("decode envelope: %w (body=%s)", err, truncate(string(body), 200))
			continue
		}
		if env.Error != nil {
			if isHeightNotAvailable(env.Error) {
				return fmt.Errorf("%w: %w: rpc code=%d %s", ErrRPCDeterministic, ErrHeightNotAvailable, env.Error.Code, env.Error.Message)
			}
			return fmt.Errorf("%w: rpc code=%d %s (%s)", ErrRPCDeterministic, env.Error.Code, env.Error.Message, env.Error.Data)
		}
		if out != nil {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
		}
		return nil
	}
	return fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
}

// isHeightNotAvailable recognises CometBFT's response when a caller asks
// for a block the node hasn't committed yet. Both the structured code
// and the message text are checked because the message wording is the
// stable bit across versions.
func isHeightNotAvailable(e *RPCError) bool {
	msg := strings.ToLower(e.Message + " " + e.Data)
	switch {
	case strings.Contains(msg, "must be less than or equal to"):
		return true
	case strings.Contains(msg, "height must be greater than"):
		return true
	case strings.Contains(msg, "could not find results for height"),
		strings.Contains(msg, "failed to load block"),
		strings.Contains(msg, "no block with that hash"),
		strings.Contains(msg, "block not found"),
		strings.Contains(msg, "height is not available"):
		return true
	}
	return false
}

// Status fetches /status from any healthy endpoint and updates the
// selected endpoint's cached status so subsequent height-bound requests
// can skip it cleanly when it lags.
func (c *Client) Status(ctx context.Context) (*StatusResult, error) {
	r, err := c.statusFromAny(ctx)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// StatusOf fetches /status from a specific endpoint URL (used by the
// doctor to probe each configured host independently). Returns
// ErrUnknownEndpoint if url isn't in the configured list.
func (c *Client) StatusOf(ctx context.Context, endpointURL string) (*StatusResult, error) {
	for _, ep := range c.endpoints {
		if ep.url == strings.TrimRight(endpointURL, "/") {
			return c.refreshStatus(ctx, ep)
		}
	}
	return nil, fmt.Errorf("endpoint %q not in client", endpointURL)
}

// statusFromAny refreshes /status on each endpoint in preference order
// and returns the first caught-up reply (catching_up=false). While a
// local primary is still syncing, this keeps the syncer's tip bound to
// the network via seed rather than the primary's in-progress height.
// Per-endpoint caches are always refreshed. If every reachable endpoint
// is still catching_up, the first reachable status is returned so the
// syncer can at least track the primary's progress.
func (c *Client) statusFromAny(ctx context.Context) (*StatusResult, error) {
	if len(c.endpoints) == 0 {
		return nil, errors.New("rpc client has no endpoints configured")
	}
	var errs []string
	var catchingUp *StatusResult
	for i, ep := range c.endpoints {
		if c.skipForRecentFailure(ep, i) {
			errs = append(errs, fmt.Sprintf("%s: skipped (recent failure: %v)", ep.url, ep.cachedErr()))
			continue
		}
		epCtx, cancel := c.endpointContext(ctx, i)
		stat, err := c.refreshStatus(epCtx, ep)
		cancel()
		if err == nil {
			if !stat.SyncInfo.CatchingUp {
				return stat, nil
			}
			if catchingUp == nil {
				catchingUp = stat
			}
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, fmt.Sprintf("%s: %v", ep.url, err))
	}
	if catchingUp != nil {
		return catchingUp, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrAllEndpointsFailed, strings.Join(errs, "; "))
}

// refreshStatus fetches /status from one endpoint, updates its cache,
// and returns the result. Errors are cached too so a flapping endpoint
// doesn't get probed every request.
func (c *Client) refreshStatus(ctx context.Context, ep *endpoint) (*StatusResult, error) {
	var r StatusResult
	err := c.getFromEndpoint(ctx, ep, "/status", &r)
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.lastSeen = time.Now()
	if err != nil {
		ep.lastErr = err
		ep.lastStat = nil
		return nil, err
	}
	ep.lastStat = &r
	ep.lastErr = nil
	return &r, nil
}

// Block returns /block?height=H, falling back across endpoints. wantHeight
// is the requested height so the selection algorithm can skip endpoints
// that haven't caught up to H yet.
func (c *Client) Block(ctx context.Context, height int64) (*BlockResult, error) {
	var r BlockResult
	if err := c.get(ctx, fmt.Sprintf("/block?height=%d", height), height, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// BlockResults returns /block_results?height=H, falling back across
// endpoints. Same wantHeight semantics as Block.
func (c *Client) BlockResults(ctx context.Context, height int64) (*BlockResultsResult, error) {
	var r BlockResultsResult
	if err := c.get(ctx, fmt.Sprintf("/block_results?height=%d", height), height, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// TxSearch issues a /tx_search request. Not height-bound, so falls back
// freely between endpoints on transient errors.
func (c *Client) TxSearch(ctx context.Context, query string, perPage int) (*TxSearchResult, error) {
	var r TxSearchResult
	q := url.QueryEscape(`"` + query + `"`)
	if err := c.get(ctx, fmt.Sprintf("/tx_search?query=%s&per_page=%d", q, perPage), 0, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Blockchain returns block metas in [minHeight..maxHeight]. CometBFT caps
// the range to 20 per call; callers should chunk. Treated as a tip-bound
// request so a catching-up endpoint with tip < maxHeight is skipped.
func (c *Client) Blockchain(ctx context.Context, minHeight, maxHeight int64) (*BlockchainResult, error) {
	var r BlockchainResult
	if err := c.get(ctx, fmt.Sprintf("/blockchain?minHeight=%d&maxHeight=%d", minHeight, maxHeight), maxHeight, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
