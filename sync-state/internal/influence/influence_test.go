package influence

import (
	"testing"

	"sync-state/internal/payload"
	"sync-state/internal/rpc"
)

func TestAmounts(t *testing.T) {
	for _, s := range []string{"", "0", "18446744073709551615"} {
		if _, e := amount(payload.Numeric(s)); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range []string{"-1", "1.5", "18446744073709551616"} {
		if _, e := amount(payload.Numeric(s)); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestRequiredAttribute(t *testing.T) {
	e := rpc.Event{Type: "raid", Attributes: []rpc.Attribute{{Key: "detail", Value: `{"x":1}`}}}
	if _, err := required(e, "detail"); err != nil {
		t.Fatal(err)
	}
	e.Attributes = append(e.Attributes, e.Attributes[0])
	if _, err := required(e, "detail"); err == nil {
		t.Fatal("duplicate")
	}
	if _, err := required(e, "absent"); err == nil {
		t.Fatal("absent")
	}
}
