package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// TipNotifier subscribes to CometBFT's WebSocket `tm.event='NewBlock'`
// stream so the syncer can wake the instant a block is committed instead
// of waiting for the next /status poll.
//
// Semantics:
//   - Best-effort. If the WebSocket can't connect (firewall, older node,
//     load balancer that drops upgrades) the notifier just stays silent
//     and the caller falls back to the poll-interval timer.
//   - Coalescing. C() is a buffered size-1 channel; if the consumer is
//     busy committing block N the notifier doesn't queue up N+1, N+2.
//     The consumer always re-reads /status anyway, so a single wake is
//     all that's needed.
//   - Reconnection. Disconnects are expected (load balancer cycles,
//     node restarts). The notifier reconnects with exponential backoff
//     and re-subscribes on every reconnect.
//   - Endpoint selection. Walks the same ordered endpoint list as
//     Client: tries the primary first; if it never delivers a NewBlock
//     within `tipSilenceTimeout`, the dial loop rotates to the next
//     endpoint. This handles "primary accepted the websocket but is
//     itself catching up so it never emits NewBlock" without forcing
//     the syncer to give up on push notifications entirely.
type TipNotifier struct {
	urls           []string
	dialTimeout    time.Duration
	reconnectMin   time.Duration
	reconnectMax   time.Duration
	pingInterval   time.Duration
	silenceTimeout time.Duration
	logger         *log.Logger

	ch chan int64

	mu        sync.Mutex
	connected bool
	lastEvent time.Time
}

// NewTipNotifier constructs a notifier over the same endpoint list as
// rpc.Client. Pass nil logger to silence the package; otherwise it logs
// at INFO level on connect / disconnect / fallover.
func NewTipNotifier(urls []string, logger *log.Logger) *TipNotifier {
	return &TipNotifier{
		urls:           dedupeURLs(urls),
		dialTimeout:    10 * time.Second,
		reconnectMin:   500 * time.Millisecond,
		reconnectMax:   30 * time.Second,
		pingInterval:   25 * time.Second,
		silenceTimeout: 60 * time.Second,
		logger:         logger,
		ch:             make(chan int64, 1),
	}
}

// C returns the wake channel. The value is the height of the new block;
// callers should re-fetch /status to get the canonical tip (a single
// wake might mean N+2 blocks arrived).
func (n *TipNotifier) C() <-chan int64 { return n.ch }

// Connected reports whether the notifier currently has a live WebSocket
// subscription. Useful for the doctor probe and runtime metrics.
func (n *TipNotifier) Connected() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.connected
}

// Run blocks until ctx is cancelled, maintaining a live WebSocket
// subscription with exponential backoff across reconnects. Safe to
// launch as `go notifier.Run(ctx)`.
func (n *TipNotifier) Run(ctx context.Context) {
	if len(n.urls) == 0 {
		n.logf("notifier disabled: no rpc endpoints configured")
		return
	}

	epIdx := 0
	backoff := n.reconnectMin
	for {
		if err := ctx.Err(); err != nil {
			return
		}

		wsURL, err := httpToWebsocket(n.urls[epIdx])
		if err != nil {
			n.logf("notifier endpoint %s unusable: %v (skipping)", n.urls[epIdx], err)
			epIdx = (epIdx + 1) % len(n.urls)
			continue
		}

		err = n.runOnce(ctx, wsURL)
		n.setConnected(false)
		if ctx.Err() != nil {
			return
		}

		// Decide whether this endpoint is worth retrying or whether
		// to fail over to the next one. We rotate when:
		//  - dial / handshake fails (network or upgrade rejected), OR
		//  - the connection went silent (no NewBlock within timeout)
		// We DON'T rotate on a clean read error after recent activity:
		// load balancers cycle long-lived connections and a fresh
		// dial to the same primary usually works.
		shouldRotate := errors.Is(err, errNotifierSilent) || errors.Is(err, errNotifierDialFailed)
		if shouldRotate && len(n.urls) > 1 {
			nextIdx := (epIdx + 1) % len(n.urls)
			n.logf("notifier rotating endpoint %s -> %s (reason: %v)", n.urls[epIdx], n.urls[nextIdx], err)
			epIdx = nextIdx
			backoff = n.reconnectMin
		} else {
			n.logf("notifier disconnected from %s: %v (reconnecting in %s)", n.urls[epIdx], err, backoff)
		}

		if !sleep(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > n.reconnectMax {
			backoff = n.reconnectMax
		}
	}
}

var (
	errNotifierSilent     = errors.New("no NewBlock events received within silence timeout")
	errNotifierDialFailed = errors.New("websocket dial failed")
)

// runOnce dials, subscribes, and reads frames until the connection
// dies or the context is cancelled. Returns the cause so the outer
// loop can decide whether to rotate endpoints.
func (n *TipNotifier) runOnce(ctx context.Context, wsURL string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: n.dialTimeout,
		Proxy:            http.ProxyFromEnvironment,
	}

	dialCtx, cancel := context.WithTimeout(ctx, n.dialTimeout)
	defer cancel()
	conn, _, err := dialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", errNotifierDialFailed, err)
	}
	defer conn.Close()

	subscribe := map[string]any{
		"jsonrpc": "2.0",
		"method":  "subscribe",
		"id":      1,
		"params":  map[string]any{"query": "tm.event='NewBlock'"},
	}
	if err := conn.WriteJSON(subscribe); err != nil {
		return fmt.Errorf("subscribe write: %w", err)
	}

	n.setConnected(true)
	n.touch()
	n.logf("notifier connected to %s (subscribed NewBlock)", wsURL)

	// Pump pings on a separate goroutine so a chatty CometBFT keeps
	// the connection warm through any intermediate load balancers.
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go n.pingLoop(pingCtx, conn)

	// Watchdog: if no NewBlock arrives for silenceTimeout, force a
	// rotation. This catches the "websocket alive but node is catching
	// up so no NewBlock is emitted" case.
	silenceCtx, silenceCancel := context.WithCancel(ctx)
	defer silenceCancel()
	go n.silenceWatchdog(silenceCtx, conn)

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if n.silenceExceeded() {
				return errNotifierSilent
			}
			return fmt.Errorf("ws read: %w", err)
		}
		height, ok := parseNewBlockHeight(raw)
		if !ok {
			continue
		}
		n.touch()
		// Non-blocking publish: a busy consumer doesn't need a queue
		// of heights; one wake-up is enough to make it re-poll /status.
		select {
		case n.ch <- height:
		default:
		}
	}
}

func (n *TipNotifier) pingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(n.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Use SetWriteDeadline so a stuck conn doesn't lock the
			// writer goroutine forever. CometBFT replies to pings
			// itself; the goal is just to keep middleboxes alive.
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				// Read loop will surface the error; just bail.
				return
			}
		}
	}
}

func (n *TipNotifier) silenceWatchdog(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(n.silenceTimeout / 4)
	if n.silenceTimeout < 4*time.Second {
		t = time.NewTicker(time.Second)
	}
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n.silenceExceeded() {
				// Forcing the connection closed makes the read loop
				// return immediately with a wrapped errNotifierSilent.
				_ = conn.Close()
				return
			}
		}
	}
}

func (n *TipNotifier) silenceExceeded() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lastEvent.IsZero() {
		return false
	}
	return time.Since(n.lastEvent) > n.silenceTimeout
}

func (n *TipNotifier) touch() {
	n.mu.Lock()
	n.lastEvent = time.Now()
	n.mu.Unlock()
}

func (n *TipNotifier) setConnected(v bool) {
	n.mu.Lock()
	n.connected = v
	if !v {
		// On disconnect, reset the silence clock so a fresh
		// connection gets a full grace period before the watchdog
		// fires again.
		n.lastEvent = time.Time{}
	}
	n.mu.Unlock()
}

func (n *TipNotifier) logf(format string, args ...any) {
	if n.logger == nil {
		return
	}
	n.logger.Printf(format, args...)
}

// httpToWebsocket converts an http(s):// CometBFT RPC URL into its
// ws(s):///websocket counterpart. Returns an error for unsupported
// schemes so the caller can skip the endpoint.
func httpToWebsocket(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already a websocket URL
	default:
		return "", fmt.Errorf("unsupported scheme %q (expected http(s) or ws(s))", u.Scheme)
	}
	if !strings.HasSuffix(u.Path, "/websocket") {
		u.Path = strings.TrimRight(u.Path, "/") + "/websocket"
	}
	return u.String(), nil
}

// parseNewBlockHeight extracts the block height from a CometBFT
// NewBlock event frame. Returns (0, false) for non-NewBlock frames
// (e.g. the initial subscription ack) so the read loop can ignore
// them silently.
func parseNewBlockHeight(raw []byte) (int64, bool) {
	// We avoid unmarshalling the entire NewBlock payload (it can be
	// huge) by pulling out only the path we need.
	var env struct {
		Result struct {
			Data struct {
				Type  string `json:"type"`
				Value struct {
					Block struct {
						Header struct {
							Height string `json:"height"`
						} `json:"header"`
					} `json:"block"`
				} `json:"value"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, false
	}
	// CometBFT versions disagree on the Type tag:
	//  - v0.34/v0.37: "tendermint/event/NewBlock"
	//  - some forks:  "tendermint/abci.EventDataNewBlock"
	// Both end in "NewBlock"; we accept either.
	if !strings.HasSuffix(env.Result.Data.Type, "NewBlock") {
		return 0, false
	}
	h, err := strconv.ParseInt(env.Result.Data.Value.Block.Header.Height, 10, 64)
	if err != nil || h <= 0 {
		return 0, false
	}
	return h, true
}

func dedupeURLs(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, u := range in {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
