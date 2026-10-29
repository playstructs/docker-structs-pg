package events

import (
	"encoding/json"
	"testing"

	"sync-state/internal/payload"
	"sync-state/internal/rpc"
)

func TestEnvelopeTypedValuesAndEmptyReset(t *testing.T) {
	ev := rpc.Event{Type: "structs.structs.EventReactorStakeCap", Attributes: []rpc.Attribute{
		{Key: "active", Value: "false"}, {Key: "capBps", Value: `"0"`},
		{Key: "minValidators", Value: `"18446744073709551615"`}, {Key: "mode", Value: "EndBlock"},
	}}
	raw, err := Envelope(ev)
	if err != nil {
		t.Fatal(err)
	}
	p, err := payload.DecodeRequired[payload.ReactorStakeCap](raw, "active", "capBps", "minValidators")
	if err != nil || p.Active || p.MinValidators.String() != "18446744073709551615" {
		t.Fatalf("payload=%+v err=%v", p, err)
	}
	raw, err = Envelope(rpc.Event{Attributes: []rpc.Attribute{{Key: "mode", Value: "BeginBlock"}}})
	if err != nil || string(raw) != "{}" {
		t.Fatalf("empty reset=%s %v", raw, err)
	}
	for _, attrs := range [][]rpc.Attribute{
		{{Key: "active", Value: "false"}, {Key: "active", Value: "true"}},
		{{Key: "reactorId", Value: "3-1"}},
	} {
		if _, err := Envelope(rpc.Event{Attributes: attrs}); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	if _, err := payload.DecodeRequired[payload.ReactorStakeCap](json.RawMessage(`null`), "active"); err == nil {
		t.Fatal("null snapshot accepted")
	}
}

func TestNestedEventRequiresExactlyOnePayload(t *testing.T) {
	router := NewRouter(false)
	for _, attrs := range [][]rpc.Attribute{nil, {{Key: "mode", Value: "BeginBlock"}},
		{{Key: "eventVaultAddressDetail", Value: "{}"}, {Key: "eventVaultAddressDetail", Value: "{}"}}} {
		if _, err := router.ValidateNestedEvent(rpc.Event{Type: "structs.structs.EventVaultAddress", Attributes: attrs}); err == nil {
			t.Fatal("missing/duplicate required nested payload accepted")
		}
	}
	if _, err := router.ValidateNestedEvent(rpc.Event{Type: "structs.structs.EventOwnerReactorDiversityReset"}); err != nil {
		t.Fatal("empty whole-event reset treated as a missing nested payload")
	}
}
