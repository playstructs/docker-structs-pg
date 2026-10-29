package rpc

import (
	"encoding/base64"
	"fmt"
	"strconv"
)

// ValidateBundle binds independently fetched block and result responses to the
// requested chain/height before any source identities or database writes exist.
// It is a shape/provenance check, not a consensus proof between RPC endpoints.
func ValidateBundle(block *BlockResult, results *BlockResultsResult, chainID string, height int64) error {
	if block == nil || results == nil || height <= 0 || chainID == "" {
		return fmt.Errorf("invalid block bundle request at height %d", height)
	}
	if block.Block.Header.ChainID != chainID {
		return fmt.Errorf("block %d chain_id=%q != live %q", height, block.Block.Header.ChainID, chainID)
	}
	for _, source := range []struct{ name, height string }{
		{"block", block.Block.Header.Height}, {"block_results", results.Height},
	} {
		got, err := strconv.ParseInt(source.height, 10, 64)
		if err != nil || got != height {
			return fmt.Errorf("block %d returned %s.height=%q", height, source.name, source.height)
		}
	}
	if len(block.Block.Data.Txs) != len(results.TxsResults) {
		return fmt.Errorf("block %d has %d transactions but %d results", height, len(block.Block.Data.Txs), len(results.TxsResults))
	}
	for i, encoded := range block.Block.Data.Txs {
		decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || len(decoded) == 0 {
			return fmt.Errorf("block %d transaction %d has invalid or empty base64 bytes", height, i)
		}
	}
	return nil
}
