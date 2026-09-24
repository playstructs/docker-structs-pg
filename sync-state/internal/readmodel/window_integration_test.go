package readmodel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
)

type windowBlock struct {
	height int64
	ledger []windowLedgerRow
}

type windowLedgerRow struct {
	address, amount, action, direction, denom string
}

func (b windowBlock) apply(t *testing.T, ctx context.Context, tx pgx.Tx) (*Dirty, []buffers.LedgerDelta) {
	t.Helper()
	d := NewDirty()
	var deltas []buffers.LedgerDelta
	for _, r := range b.ledger {
		seedIsolatedLedger(t, ctx, tx, r.address, r.amount, r.action, r.direction, r.denom, b.height)
		delta := r.amount
		if r.direction == "debit" {
			delta = "-" + r.amount
		}
		deltas = append(deltas, buffers.LedgerDelta{Address: r.address, Denom: r.denom, Delta: delta})
		d.Address(r.address)
	}
	return d, deltas
}

func projectionSnapshot(t *testing.T, ctx context.Context, tx pgx.Tx, guild string, players, addresses []string) string {
	t.Helper()
	var snap string
	if err := tx.QueryRow(ctx, `
SELECT jsonb_build_object(
  'inventory', (SELECT COALESCE(jsonb_agg(to_jsonb(i) ORDER BY owner_type, owner_id, denom), '[]')
                  FROM structs.api_inventory i
                 WHERE (owner_type='player' AND owner_id = ANY($2::varchar[]))
                    OR (owner_type='address' AND owner_id = ANY($3::varchar[]))),
  'players',   (SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY player_id), '[]')
                  FROM structs.api_leaderboard_player p WHERE player_id = ANY($2::varchar[])),
  'guild',     (SELECT COALESCE(jsonb_agg(to_jsonb(g)), '[]')
                  FROM structs.api_leaderboard_guild g WHERE guild_id = $1),
  'bank',      (SELECT COALESCE(jsonb_agg(to_jsonb(b) ORDER BY denom), '[]')
                  FROM structs.api_guild_bank b WHERE guild_id = $1)
)::text`, guild, players, addresses).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

// A bulk window merges every block's dirty set and ledger deltas and
// recomputes once; the projections must match per-block recompute.
func TestWindowRecomputeMatchesPerBlock(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		guild := "0-991130"
		denom := "uguild." + guild
		p1, p2 := "1-991131", "1-991132"
		a1, a2 := "structs1isowin1", "structs1isowin2"
		seedIsolatedGuild(t, ctx, tx, guild, "iso-window")
		seedIsolatedPlayer(t, ctx, tx, p1, "iso-win-1", guild, "")
		seedIsolatedPlayer(t, ctx, tx, p2, "iso-win-2", guild, "")
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.player_address (address, player_id, status, created_at, updated_at) VALUES
  ($1, $3, 'approved', NOW(), NOW()),
  ($2, $4, 'approved', NOW(), NOW())
ON CONFLICT (address) DO UPDATE SET player_id = EXCLUDED.player_id`, a1, a2, p1, p2); err != nil {
			t.Fatal(err)
		}
		blocks := []windowBlock{
			{height: 900000001, ledger: []windowLedgerRow{
				{a1, "100", "received", "credit", "ualpha"},
				{a2, "50", "minted", "credit", denom},
			}},
			{height: 900000002, ledger: []windowLedgerRow{
				{a1, "30", "sent", "debit", "ualpha"},
				{a2, "20", "sent", "debit", denom},
				{a1, "20", "received", "credit", denom},
			}},
			{height: 900000003, ledger: []windowLedgerRow{
				{a1, "20", "sent", "debit", denom},
				{a2, "20", "received", "credit", denom},
			}},
		}
		last := blocks[len(blocks)-1].height
		when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		players, addresses := []string{p1, p2}, []string{a1, a2}

		if _, err := tx.Exec(ctx, `SAVEPOINT window_equivalence`); err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			d, deltas := b.apply(t, ctx, tx)
			if err := Recompute(ctx, tx, d, b.height, when, InventoryFromDeltas(deltas)); err != nil {
				t.Fatalf("per-block recompute at %d: %v", b.height, err)
			}
		}
		perBlock := projectionSnapshot(t, ctx, tx, guild, players, addresses)
		for _, want := range []string{`"owner_id": "` + p1 + `"`, `"owner_id": "` + a2 + `"`, `"player_id": "` + p2 + `"`, `"guild_id": "` + guild + `"`} {
			if !strings.Contains(perBlock, want) {
				t.Fatalf("per-block snapshot missing %s: %s", want, perBlock)
			}
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT window_equivalence`); err != nil {
			t.Fatal(err)
		}

		window := NewDirty()
		var windowDeltas []buffers.LedgerDelta
		for _, b := range blocks {
			d, deltas := b.apply(t, ctx, tx)
			window.Merge(d)
			windowDeltas = append(windowDeltas, deltas...)
		}
		if err := Recompute(ctx, tx, window, last, when, InventoryFromDeltas(windowDeltas)); err != nil {
			t.Fatalf("window recompute: %v", err)
		}
		windowed := projectionSnapshot(t, ctx, tx, guild, players, addresses)
		if perBlock != windowed {
			t.Fatalf("window recompute diverged\nper-block: %s\nwindow:    %s", perBlock, windowed)
		}
	})
}

// Players reached only through guild-token holdings get their leaderboard
// row recomputed but their api_inventory rows are left alone.
func TestGuildTokenFanOutSkipsInventoryRebuild(t *testing.T) {
	integrationTx(t, func(ctx context.Context, tx pgx.Tx) {
		guild := "0-991140"
		denom := "uguild." + guild
		holder := "1-991141"
		seedIsolatedGuild(t, ctx, tx, guild, "iso-fanout")
		seedIsolatedPlayer(t, ctx, tx, holder, "iso-fanout", "", "")
		if _, err := tx.Exec(ctx, `
INSERT INTO structs.api_inventory(owner_type, owner_id, denom, balance)
VALUES ('player', $1, $2, 777)
ON CONFLICT DO NOTHING`, holder, denom); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM structs.api_leaderboard_player WHERE player_id=$1`, holder); err != nil {
			t.Fatal(err)
		}
		d := NewDirty()
		d.Guild(guild)
		if err := Recompute(ctx, tx, d, 900000010, time.Now().UTC(), InventoryFromDeltas(nil)); err != nil {
			t.Fatal(err)
		}
		if _, ok := d.Players[holder]; !ok {
			t.Fatalf("guild token holder %s not expanded into Players", holder)
		}
		for _, id := range d.InventoryPlayerIDs() {
			if id == holder {
				t.Fatalf("guild token holder %s scheduled for inventory rebuild", holder)
			}
		}
		var bal string
		if err := tx.QueryRow(ctx, `
SELECT balance::text FROM structs.api_inventory
WHERE owner_type='player' AND owner_id=$1 AND denom=$2`, holder, denom).Scan(&bal); err != nil {
			t.Fatal(err)
		}
		if bal != "777" {
			t.Fatalf("fan-out rewrote holder inventory: balance=%s", bal)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM structs.api_leaderboard_player WHERE player_id=$1`, holder).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("fan-out did not recompute holder leaderboard row (rows=%d)", n)
		}
	})
}
