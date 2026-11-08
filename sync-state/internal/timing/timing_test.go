package timing

import (
	"context"
	"testing"
)

func TestNestedAndRepeatedPhases(t *testing.T) {
	ctx, r := New(context.Background())
	parent, child := Phases(ctx), Phases(ctx)
	parent("parent")
	child("child")
	child("")
	first := r.Durations["child"]
	child("child")
	child("")
	parent("")
	if r.Durations["child"] < first || r.Durations["parent"] < r.Durations["child"] {
		t.Fatalf("invalid nested durations: %v", r.Durations)
	}
	Count(ctx, "rows", 2)
	Count(ctx, "rows", 3)
	if r.Counts["rows"] != 5 {
		t.Fatal(r.Counts)
	}
	other, separate := New(ctx)
	Count(other, "rows", 9)
	if r.Counts["rows"] != 5 || separate.Counts["rows"] != 9 {
		t.Fatal("recorders share state")
	}
	Phases(context.Background())("disabled")
	Count(context.Background(), "disabled", 1)
}
