// Package lcd reads complete snapshots from the configured Cosmos gateway.
// A requested height is a requirement, not a hint or a latest-height fallback.
package lcd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func (c Client) get(ctx context.Context, path string, height int64, out any) error {
	if c.HTTP == nil {
		return fmt.Errorf("LCD snapshot requires an HTTP client with a request timeout")
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return fmt.Errorf("LCD path must be local to the configured gateway")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	if height > 0 {
		req.Header.Set("x-cosmos-block-height", strconv.FormatInt(height, 10))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("LCD %s: HTTP %d at height %d", path, resp.StatusCode, height)
	}
	if height > 0 {
		found := false
		for _, header := range []string{"X-Cosmos-Block-Height", "Grpc-Metadata-X-Cosmos-Block-Height"} {
			for _, value := range resp.Header.Values(header) {
				found = true
				actual, err := strconv.ParseInt(value, 10, 64)
				if err != nil || actual != height {
					return fmt.Errorf("LCD %s: requested height %d, %s returned %q", path, height, header, value)
				}
			}
		}
		if !found {
			return fmt.Errorf("LCD %s: no response height for requested snapshot %d", path, height)
		}
	}
	const maxResponse = 32 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(body) > maxResponse {
		return fmt.Errorf("LCD page exceeds %d bytes; reduce the page limit", maxResponse)
	}
	return json.Unmarshal(body, out)
}

func (c Client) ValidateChain(ctx context.Context, chainID string) error {
	if chainID == "" {
		return fmt.Errorf("LCD snapshot needs an expected chain ID")
	}
	var info struct {
		Node struct {
			Network string `json:"network"`
		} `json:"default_node_info"`
	}
	if err := c.get(ctx, "/cosmos/base/tendermint/v1beta1/node_info", 0, &info); err != nil {
		return err
	}
	if info.Node.Network != chainID {
		return fmt.Errorf("LCD chain mismatch: expected %s, received %s", chainID, info.Node.Network)
	}
	return nil
}

// Get requires a positive, explicit committed height and validates the actual
// gateway response height. Call ValidateChain once before a snapshot session.
func (c Client) Get(ctx context.Context, path string, height int64, out any) error {
	if height <= 0 {
		return fmt.Errorf("LCD snapshot height must be positive")
	}
	return c.get(ctx, path, height, out)
}

// Pages follows next_key even after an empty page. Missing collection or
// pagination envelopes, invalid rows, repeated keys and any failed/mismatched
// page reject the whole snapshot. The visitor must stage rows, never publish a
// partial database repair before Pages returns successfully.
func (c Client) Pages(ctx context.Context, path, field string, height int64, limit int, visit func(json.RawMessage) error) error {
	if limit <= 0 {
		return fmt.Errorf("LCD page limit must be positive")
	}
	seen := map[string]bool{}
	key := ""
	for pageNumber := 0; pageNumber < 10000; pageNumber++ {
		query := url.Values{"pagination.limit": {strconv.Itoa(limit)}}
		if key != "" {
			query.Set("pagination.key", key)
		}
		var envelope map[string]json.RawMessage
		if err := c.Get(ctx, path+"?"+query.Encode(), height, &envelope); err != nil {
			return err
		}
		var rows []json.RawMessage
		if raw := envelope[field]; len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("LCD %s: missing collection %s", path, field)
		} else if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		var pagination *struct {
			NextKey string `json:"next_key"`
		}
		if err := json.Unmarshal(envelope["pagination"], &pagination); err != nil || pagination == nil {
			return fmt.Errorf("LCD %s: missing or malformed pagination", path)
		}
		for _, row := range rows {
			if err := visit(row); err != nil {
				return fmt.Errorf("LCD %s row: %w", path, err)
			}
		}
		key = pagination.NextKey
		if key == "" {
			return nil
		}
		if seen[key] {
			return fmt.Errorf("LCD %s: repeated pagination key", path)
		}
		seen[key] = true
	}
	return fmt.Errorf("LCD %s: snapshot exceeds 10000 pages; increase the page limit", path)
}
