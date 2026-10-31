package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sync-state/internal/rpc"
)

func TestTxIndexerChecksOnlyOneBlock(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "enabled_empty_block"
		wantSeverity := OK
		if disabled {
			name = "disabled_indexer"
			wantSeverity = FATAL
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/tx_search" || r.URL.Query().Get("query") != `"tx.height=3366389"` {
					t.Errorf("probe must query exactly the observed tip: %s", r.URL)
				}
				if disabled {
					rpcErr(w, "transaction indexing is disabled")
					return
				}
				ok(w, `{"txs":[],"total_count":"0"}`)
			}))
			defer server.Close()

			client := rpc.NewClient([]string{server.URL}, time.Second, 0)
			check := probeTxIndexer(context.Background(), client, 3366389)
			if check == nil || check.Severity != wantSeverity {
				t.Fatalf("unexpected indexer verdict: %+v", check)
			}
			if requests != 1 {
				t.Fatalf("expected one bounded query, got %d", requests)
			}
		})
	}
}
