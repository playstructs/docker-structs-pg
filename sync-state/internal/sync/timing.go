package sync

import (
	"context"
	"encoding/json"
	"time"

	"sync-state/internal/timing"
)

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Log only bounded metadata, never payloads, addresses, SQL or error messages.
// Bulk fetches overlap; their summed time is deliberately separate from apply.
func (s *Syncer) startIngestTiming(ctx context.Context, blocks []*BlockBundle, bulk bool) (context.Context, func(string)) {
	if s.cfg.SlowBlockThreshold <= 0 {
		return ctx, func(string) {}
	}
	started := time.Now()
	ctx, recorder := timing.New(ctx)
	return ctx, func(outcome string) {
		apply := time.Since(started)
		var fetch time.Duration
		var transactions, events int
		for _, b := range blocks {
			fetch += b.FetchDuration
			transactions += len(b.TxResults)
			events += len(b.FinalizeBlockEvents)
			for _, tx := range b.TxResults {
				events += len(tx.Events)
			}
		}
		active := apply
		if !bulk {
			active += fetch
		}
		if active < s.cfg.SlowBlockThreshold {
			return
		}
		phases := make(map[string]float64, len(recorder.Durations))
		for name, duration := range recorder.Durations {
			phases[name] = millis(duration)
		}
		report := map[string]any{
			"event": "slow_block", "height_start": blocks[0].Height,
			"height_end": blocks[len(blocks)-1].Height, "blocks": len(blocks),
			"transactions": transactions, "input_events": events, "outcome": outcome,
			"apply_ms": millis(apply), "phases_ms": phases, "counts": recorder.Counts,
		}
		if bulk {
			report["event"] = "slow_window"
			report["fetch_sum_ms"] = millis(fetch)
		} else {
			report["fetch_decode_ms"] = millis(fetch)
			if at := blocks[0].FetchedAt; !at.IsZero() && started.After(at) {
				report["queue_wait_ms"] = millis(started.Sub(at))
			}
		}
		body, _ := json.Marshal(report)
		s.logger.Print(string(body))
	}
}
