package buffers

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPlanetActivitySequences_RollbackAndResume(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	const existing, fresh = "2-999910001", "2-999910002"
	if _, err := tx.Exec(ctx, "INSERT INTO structs.planet_activity_sequence(planet_id,counter) VALUES ($1,41)", existing); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	row := func(planet, marker string) PlanetActivityRow {
		detail, _ := json.Marshal(map[string]string{"marker": marker})
		return PlanetActivityRow{Time: now, PlanetID: planet, Category: "shield_change", Detail: detail, BlockHeight: 999910001}
	}
	b := New()
	b.PlanetActivity = []PlanetActivityRow{row(existing, "a"), row(fresh, "b")}
	// A failed handler's appended rows are removed before reservation.
	snapshot := b.Snapshot()
	b.PlanetActivity = append(b.PlanetActivity, row(fresh, "discarded"))
	b.Restore(snapshot)
	b.PlanetActivity = append(b.PlanetActivity, row(existing, "c"), row(fresh, "d"))

	// COPY fails after sequence reservation. Roll back its savepoint and retry
	// the retained buffer, as the outer block/window would on restart.
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.PlanetActivity[3].Category = "invalid_activity_category"
	if _, err := b.Flush(ctx, sp); err == nil {
		t.Fatal("expected COPY failure")
	}
	if err := sp.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if len(b.PlanetActivity) != 4 {
		t.Fatalf("failed flush lost buffered rows: %d", len(b.PlanetActivity))
	}
	b.PlanetActivity[3].Category = "shield_change"
	if _, err := b.Flush(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if len(b.PlanetActivity) != 0 {
		t.Fatal("successful flush retained rows")
	}
	// A new buffer resumes from the transaction's counters for the next block.
	next := New()
	next.PlanetActivity = []PlanetActivityRow{row(fresh, "e"), row(existing, "f")}
	if _, err := next.Flush(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var got []int
	if err := tx.QueryRow(ctx, `SELECT array_agg(seq ORDER BY detail->>'marker')
		FROM structs.planet_activity WHERE planet_id=ANY($1)`, []string{existing, fresh}).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := []int{42, 0, 43, 1, 2, 44}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence order = %v, want %v", got, want)
	}
	var counters []int
	if err := tx.QueryRow(ctx, `SELECT array_agg(counter ORDER BY planet_id)
		FROM structs.planet_activity_sequence WHERE planet_id=ANY($1)`, []string{existing, fresh}).Scan(&counters); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counters, []int{44, 2}) {
		t.Fatalf("counters = %v", counters)
	}
}

func TestPlanetActivitySequences_CounterOverflowRollsBack(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	const planet = "2-999910003"
	if _, err := tx.Exec(ctx, "INSERT INTO structs.planet_activity_sequence VALUES ($1,2147483647)", planet); err != nil {
		t.Fatal(err)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.PlanetActivity = []PlanetActivityRow{{PlanetID: planet}}
	if _, err := b.Flush(ctx, sp); err == nil {
		t.Fatal("expected exhausted counter to reject activity")
	}
	if err := sp.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var counter int64
	if err := tx.QueryRow(ctx, "SELECT counter FROM structs.planet_activity_sequence WHERE planet_id=$1", planet).Scan(&counter); err != nil || counter != 2147483647 {
		t.Fatalf("counter after rollback = %d, err=%v", counter, err)
	}
}
