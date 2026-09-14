package events

import (
	"context"
	"encoding/json"
	"testing"
)

func TestIgnoreUnknown(t *testing.T) {
	yes := []string{
		"commission.validator",
		"rewards.validator",
		"rewards.mode",
		"commission.mode",
		"rewards.amount",
		"cosmos.authz.v1beta1.EventGrant.grantee",
		"ibc.core.client.v1.EventCreateClient.client_id",
	}
	for _, k := range yes {
		if !IgnoreUnknown(k) {
			t.Errorf("IgnoreUnknown(%q) = false, want true", k)
		}
	}
	no := []string{
		"structs.structs.EventDelete.objectId",
		"transfer.recipient",
		"coinbase.minter",
		"message.action",
	}
	for _, k := range no {
		if IgnoreUnknown(k) {
			t.Errorf("IgnoreUnknown(%q) = true, want false", k)
		}
	}
}

func TestDispatchIgnoresDistributionAttributes(t *testing.T) {
	r := NewRouter(false)
	he, fatal := r.Dispatch(context.Background(), nil, BlockContext{Height: 10}, "rewards.validator", json.RawMessage(`"x"`))
	if he != nil || fatal != nil {
		t.Fatalf("ignored unknown returned he=%v fatal=%v", he, fatal)
	}
	total, byKey := r.UnknownSummary()
	if total != 0 || len(byKey) != 0 {
		t.Fatalf("ignored key was counted: total=%d byKey=%v", total, byKey)
	}
	if deltas := r.DrainUnknownDeltas(); len(deltas) != 0 {
		t.Fatalf("ignored key produced deltas: %v", deltas)
	}
}
