package rpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// GenesisEnvelope is what /genesis returns: { genesis: <full_genesis_doc> }.
// On chains whose genesis exceeds CometBFT's max_body_bytes (default 1 MiB
// post-v0.34, historically much smaller), the RPC fails with a "genesis
// response is too large" error and the caller must switch to
// /genesis_chunked. Genesis() handles that fallback transparently.
type GenesisEnvelope struct {
	Genesis json.RawMessage `json:"genesis"`
}

// GenesisChunkResult is the per-chunk reply from /genesis_chunked?chunk=N.
// `data` is base64; concatenating decoded(data) across chunks 0..total-1
// yields the full genesis JSON document.
type GenesisChunkResult struct {
	Chunk string `json:"chunk"`
	Total string `json:"total"`
	Data  string `json:"data"`
}

// Genesis fetches the chain's genesis JSON document, transparently
// falling back to the chunked endpoint if /genesis says the payload is
// too large. The returned bytes are the raw genesis JSON (the contents of
// `result.genesis` for non-chunked, or the concatenated decoded chunks
// for chunked).
//
// Why one entry point: callers want "give me the genesis" not "first try
// X then try Y" — the size threshold that flips a chain from /genesis to
// /genesis_chunked is operator-tunable, so even small testnets can hit
// it. The fallback condition checks both the structured error code AND
// the message text because CometBFT versions disagree on the wording.
func (c *Client) Genesis(ctx context.Context) ([]byte, error) {
	var env GenesisEnvelope
	err := c.get(ctx, "/genesis", 0, &env)
	if err == nil {
		if len(env.Genesis) == 0 {
			return nil, errors.New("rpc: /genesis returned empty payload")
		}
		return env.Genesis, nil
	}
	if !isGenesisTooLarge(err) {
		return nil, fmt.Errorf("rpc /genesis: %w", err)
	}
	return c.genesisChunked(ctx)
}

// genesisChunked walks /genesis_chunked?chunk=0..total-1, base64-decodes
// each `data` field, and concatenates them in order. Total is read from
// chunk 0 so a single round-trip suffices for tiny genesis files.
func (c *Client) genesisChunked(ctx context.Context) ([]byte, error) {
	first, err := c.fetchGenesisChunk(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("rpc /genesis_chunked chunk=0: %w", err)
	}
	total, err := strconv.Atoi(first.Total)
	if err != nil || total <= 0 {
		return nil, fmt.Errorf("rpc /genesis_chunked: invalid total=%q", first.Total)
	}
	buf, err := base64.StdEncoding.DecodeString(first.Data)
	if err != nil {
		return nil, fmt.Errorf("rpc /genesis_chunked chunk=0 decode: %w", err)
	}
	out := make([]byte, 0, len(buf)*total)
	out = append(out, buf...)
	for i := 1; i < total; i++ {
		ch, err := c.fetchGenesisChunk(ctx, i)
		if err != nil {
			return nil, fmt.Errorf("rpc /genesis_chunked chunk=%d: %w", i, err)
		}
		decoded, err := base64.StdEncoding.DecodeString(ch.Data)
		if err != nil {
			return nil, fmt.Errorf("rpc /genesis_chunked chunk=%d decode: %w", i, err)
		}
		out = append(out, decoded...)
	}
	return out, nil
}

func (c *Client) fetchGenesisChunk(ctx context.Context, n int) (*GenesisChunkResult, error) {
	var r GenesisChunkResult
	if err := c.get(ctx, fmt.Sprintf("/genesis_chunked?chunk=%d", n), 0, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// isGenesisTooLarge recognises the RPC envelope error that tells us to
// switch to /genesis_chunked. Wording has drifted across CometBFT/
// Tendermint versions, so we match on substrings of the message rather
// than a single canonical string.
func isGenesisTooLarge(err error) bool {
	if err == nil || !errors.Is(err, ErrRPCDeterministic) {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "genesis response is large"),
		strings.Contains(msg, "use the genesis_chunked"),
		strings.Contains(msg, "genesis_chunked api"),
		strings.Contains(msg, "genesis chunked"):
		return true
	}
	return false
}
