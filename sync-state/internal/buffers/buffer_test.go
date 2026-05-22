package buffers

import (
	"testing"
	"time"
)

func TestSnapshotRestore_TruncatesPostSnapshotRows(t *testing.T) {
	b := New()
	b.Ledger = append(b.Ledger, LedgerRow{Address: "a"})
	snap := b.Snapshot()
	b.Ledger = append(b.Ledger, LedgerRow{Address: "b"})
	b.PlanetActivity = append(b.PlanetActivity, PlanetActivityRow{PlanetID: "p1", Seq: 1})

	if got := len(b.Ledger); got != 2 {
		t.Fatalf("Ledger len pre-restore = %d, want 2", got)
	}
	b.Restore(snap)
	if got := len(b.Ledger); got != 1 {
		t.Fatalf("Ledger len post-restore = %d, want 1", got)
	}
	if got := len(b.PlanetActivity); got != 0 {
		t.Fatalf("PlanetActivity len post-restore = %d, want 0", got)
	}
	if b.Ledger[0].Address != "a" {
		t.Fatalf("pre-snapshot row got truncated: %+v", b.Ledger[0])
	}
}

func TestSnapshotRestore_NilSafe(t *testing.T) {
	var b *Buffer
	if got := b.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("nil Snapshot() != zero")
	}
	b.Restore(Snapshot{})
	if b.Len() != 0 {
		t.Fatal("nil Len() != 0")
	}
}

func TestLen_SumsAllTables(t *testing.T) {
	b := New()
	now := time.Now()
	b.Ledger = []LedgerRow{{Address: "a"}, {Address: "b"}}
	b.PlanetActivity = []PlanetActivityRow{{PlanetID: "p1", Time: now}}
	b.StatOre = []StatRow{{Time: now, ObjectIndex: 1}}
	if got := b.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}
}
