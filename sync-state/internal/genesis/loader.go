package genesis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"sync-state/internal/rpc"
)

// LoadedDocument bundles a parsed Document with the raw bytes (for
// hashing/audit) and a human-readable source tag.
type LoadedDocument struct {
	Doc    *Document
	Raw    []byte
	Source string // "rpc:<url>" or "file:<path>"
	SHA256 string // hex-encoded sha256 of Raw
}

// LoadFromRPC fetches the genesis JSON from the RPC pool. Uses
// /genesis when the payload fits, transparently falling back to
// /genesis_chunked when CometBFT says the response is too large
// (see rpc.Client.Genesis). The source tag includes the URL the
// fetch landed on (the pool's first endpoint for now; the rpc.Client
// surfaces the preferred URL via BaseURL).
func LoadFromRPC(ctx context.Context, client *rpc.Client) (*LoadedDocument, error) {
	raw, err := client.Genesis(ctx)
	if err != nil {
		return nil, fmt.Errorf("genesis: rpc fetch: %w", err)
	}
	doc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return &LoadedDocument{
		Doc:    doc,
		Raw:    raw,
		Source: "rpc:" + client.BaseURL(),
		SHA256: hashHex(raw),
	}, nil
}

// LoadFromFile reads genesis JSON from disk. Used by operators who have
// the genesis file mounted into the sync-state container or who want to
// import from an air-gapped snapshot. Returns the same shape as
// LoadFromRPC so the Apply path is source-agnostic.
func LoadFromFile(path string) (*LoadedDocument, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("genesis: read %s: %w", path, err)
	}
	doc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return &LoadedDocument{
		Doc:    doc,
		Raw:    raw,
		Source: "file:" + path,
		SHA256: hashHex(raw),
	}, nil
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
