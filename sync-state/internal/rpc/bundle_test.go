package rpc

import "testing"

func TestValidateBundle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*BlockResult, *BlockResultsResult)
		wantError bool
	}{
		{"empty upgrade block", func(b *BlockResult, r *BlockResultsResult) {
			b.Block.Data.Txs = nil
			r.TxsResults = nil
			r.FinalizeBlockEvents = []Event{{Type: "structs.structs.EventReactorCommissionRevisionReset"}}
		}, false},
		{"transaction", func(*BlockResult, *BlockResultsResult) {}, false},
		{"wrong chain", func(b *BlockResult, _ *BlockResultsResult) { b.Block.Header.ChainID = "other" }, true},
		{"wrong block", func(b *BlockResult, _ *BlockResultsResult) { b.Block.Header.Height = "9" }, true},
		{"wrong results", func(_ *BlockResult, r *BlockResultsResult) { r.Height = "11" }, true},
		{"malformed results height", func(_ *BlockResult, r *BlockResultsResult) { r.Height = "10.0" }, true},
		{"missing result", func(_ *BlockResult, r *BlockResultsResult) { r.TxsResults = nil }, true},
		{"extra result", func(_ *BlockResult, r *BlockResultsResult) { r.TxsResults = append(r.TxsResults, TxResult{}) }, true},
		{"invalid bytes", func(b *BlockResult, _ *BlockResultsResult) { b.Block.Data.Txs[0] = "not base64" }, true},
		{"empty bytes", func(b *BlockResult, _ *BlockResultsResult) { b.Block.Data.Txs[0] = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &BlockResult{}
			b.Block.Header.ChainID, b.Block.Header.Height = "test-chain", "10"
			b.Block.Data.Txs = []string{"AQID"}
			r := &BlockResultsResult{Height: "10", TxsResults: []TxResult{{}}}
			tc.mutate(b, r)
			if err := ValidateBundle(b, r, "test-chain", 10); (err != nil) != tc.wantError {
				t.Fatalf("ValidateBundle error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
	if err := ValidateBundle(nil, nil, "test-chain", 10); err == nil {
		t.Fatal("nil response accepted")
	}
}
