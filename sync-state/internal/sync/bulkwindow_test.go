package sync

import (
	"testing"
	"time"
)

func TestEffectiveWindowSize(t *testing.T) {
	s := &Syncer{cfg: Config{BatchSize: 200, BulkWindow: 100}}

	if got := s.effectiveWindowSize(false); got != 200 {
		t.Fatalf("stream window = %d, want 200", got)
	}
	if got := s.effectiveWindowSize(true); got != 100 {
		t.Fatalf("bulk window = %d, want 100", got)
	}

	s.cfg.BatchSize = 50
	s.cfg.BulkWindow = 100
	if got := s.effectiveWindowSize(true); got != 100 {
		t.Fatalf("bulk window when bulk > batch = %d, want 100", got)
	}
}

func TestBulkLagThreshold(t *testing.T) {
	cfg := LoadConfig([]string{"-bulk-lag-threshold", "25"})
	if cfg.BulkLagThreshold != 25 {
		t.Fatalf("BulkLagThreshold = %d, want 25", cfg.BulkLagThreshold)
	}
	if !cfg.BulkEnabled {
		t.Fatal("BulkEnabled should default true")
	}
	if cfg.BulkWindow != 100 {
		t.Fatalf("BulkWindow default = %d, want 100", cfg.BulkWindow)
	}
	if cfg.BulkStatementTimeout != 5*time.Minute {
		t.Fatalf("BulkStatementTimeout = %s, want 5m", cfg.BulkStatementTimeout)
	}
}
