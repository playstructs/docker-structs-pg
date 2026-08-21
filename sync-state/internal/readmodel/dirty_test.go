package readmodel

import (
	"testing"

	"sync-state/internal/buffers"
)

func TestDirtySnapshotRestore(t *testing.T) {
	d := NewDirty()
	d.Player("1-1")
	snap := d.Snapshot()
	d.Player("1-2")
	d.Guild("0-7")
	d.Restore(snap)

	if _, ok := d.Players["1-1"]; !ok {
		t.Fatal("original player missing after restore")
	}
	if _, ok := d.Players["1-2"]; ok {
		t.Fatal("post-snapshot player survived restore")
	}
	if len(d.Guilds) != 0 {
		t.Fatalf("post-snapshot guilds survived restore: %v", d.Guilds)
	}
}

func TestModelOrder(t *testing.T) {
	want := []string{
		"inventory", "guild_bank", "leaderboard_player", "leaderboard_guild",
		"leaderboard_reactor", "leaderboard_provider", "leaderboard_substation",
	}
	if len(modelNames) != len(want) {
		t.Fatalf("modelNames=%v want %v", modelNames, want)
	}
	for i, name := range want {
		if modelNames[i] != name {
			t.Fatalf("modelNames[%d]=%s want %s", i, modelNames[i], name)
		}
	}
}

func TestDirtyNilSafe(t *testing.T) {
	var d *Dirty
	d.Address("a")
	d.Player("1-1")
	d.Guild("0-1")
	d.Ledger([]buffers.LedgerRow{{Address: "a", Action: "minted", Denom: "uguild.0-1"}})
	snap := d.Snapshot()
	d.Restore(snap)
	if d != nil {
		t.Fatal("nil Dirty should stay nil")
	}
}

func TestDirtyLedgerMarksAddressesAndGuildSupply(t *testing.T) {
	d := NewDirty()
	d.Ledger([]buffers.LedgerRow{
		{Address: "addr1", Action: "send", Denom: "ualpha"},
		{Address: "pool", Action: "minted", Denom: "uguild.0-9"},
		{Address: "pool", Action: "transfer", Denom: "uguild.0-8"},
	})
	if len(d.Addresses) != 2 {
		t.Fatalf("addresses=%v", d.Addresses)
	}
	if _, ok := d.Guilds["0-9"]; !ok {
		t.Fatal("minted guild denom did not dirty guild")
	}
	if _, ok := d.Guilds["0-8"]; ok {
		t.Fatal("non-supply action dirtied guild")
	}
}
