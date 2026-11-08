package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"testing"
	"time"

	"sync-state/internal/rpc"
	"sync-state/internal/timing"
)

func TestSlowTimingThresholdsAndMetadata(t *testing.T) {
	for _, tc := range []struct {
		name       string
		threshold  time.Duration
		bulk, want bool
	}{
		{"disabled", 0, false, false}, {"fast", time.Hour, false, false},
		{"fetch slow", time.Second, false, true}, {"parallel fetch sum is not apply", time.Second, true, false},
		{"bulk slow", time.Nanosecond, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			s := &Syncer{cfg: Config{SlowBlockThreshold: tc.threshold}, logger: log.New(&out, "", 0)}
			b := &BlockBundle{Height: 42, FetchDuration: 2 * time.Second, FetchedAt: time.Now().Add(-time.Second),
				FinalizeBlockEvents: []rpc.Event{{Type: "private-payload"}}, TxResults: []rpc.TxResult{{Events: []rpc.Event{{Type: "private-payload"}}}}}
			ctx, finish := s.startIngestTiming(context.Background(), []*BlockBundle{b}, tc.bulk)
			next := timing.Phases(ctx)
			next("dispatch")
			timing.Count(ctx, "rows", 7)
			next("")
			finish("committed")
			if !tc.want {
				if out.Len() != 0 {
					t.Fatal(out.String())
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["outcome"] != "committed" || got["height_start"] != float64(42) || got["input_events"] != float64(2) || got["transactions"] != float64(1) {
				t.Fatal(got)
			}
			if bytes.Contains(out.Bytes(), []byte("private-payload")) {
				t.Fatal("payload leaked")
			}
			if got["counts"].(map[string]any)["rows"] != float64(7) {
				t.Fatal(got)
			}
			if _, ok := got["phases_ms"].(map[string]any)["dispatch"]; !ok {
				t.Fatal(got)
			}
			if tc.bulk {
				if got["event"] != "slow_window" || got["fetch_sum_ms"] != float64(2000) {
					t.Fatal(got)
				}
			} else {
				if got["event"] != "slow_block" || got["fetch_decode_ms"] != float64(2000) || got["queue_wait_ms"].(float64) <= 0 {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestSlowTimingConfig(t *testing.T) {
	withEnv(t, map[string]string{"SYNC_SLOW_BLOCK_THRESHOLD": ""})
	if got := LoadConfig(nil).SlowBlockThreshold; got != 500*time.Millisecond {
		t.Fatal(got)
	}
	t.Setenv("SYNC_SLOW_BLOCK_THRESHOLD", "0s")
	if got := LoadConfig(nil).SlowBlockThreshold; got != 0 {
		t.Fatal(got)
	}
	if got := LoadConfig([]string{"-slow-block-threshold=3s"}).SlowBlockThreshold; got != 3*time.Second {
		t.Fatal(got)
	}
}

func TestDefaultTimingReportsHalfSecondFetch(t *testing.T) {
	withEnv(t, map[string]string{"SYNC_SLOW_BLOCK_THRESHOLD": ""})
	var out bytes.Buffer
	s := &Syncer{cfg: LoadConfig(nil), logger: log.New(&out, "", 0)}
	_, finish := s.startIngestTiming(context.Background(), []*BlockBundle{{Height: 42, FetchDuration: 500 * time.Millisecond}}, false)
	finish("committed")
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("default threshold did not report half-second block: %q: %v", out.String(), err)
	}
	if report["event"] != "slow_block" || report["fetch_decode_ms"] != float64(500) {
		t.Fatal(report)
	}
}
