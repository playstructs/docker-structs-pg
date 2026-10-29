package bank

import (
	"testing"

	"sync-state/internal/rpc"
)

func nativeEvent(kind string, pairs ...string) rpc.Event {
	ev := rpc.Event{Type: kind}
	for i := 0; i < len(pairs); i += 2 {
		ev.Attributes = append(ev.Attributes, rpc.Attribute{Key: pairs[i], Value: pairs[i+1]})
	}
	return ev
}

func TestNativeChanges_EmptyCompletionAndPoolExposure(t *testing.T) {
	for _, kind := range []string{"delegate", "unbond", "cancel_unbond", "complete_unbonding"} {
		t.Run(kind, func(t *testing.T) {
			ev := nativeEvent(kind, "delegator", "owner", "validator", "operator", "amount", "", "completion_time", "2000-01-01T00:00:00Z")
			got, err := NativeChanges([]rpc.Event{ev}, nil)
			if err != nil || len(got.Addresses) != 1 || got.Addresses[0] != "owner" || len(got.Validators) != 1 || got.Validators[0] != "operator" || got.AllExposed {
				t.Fatalf("changes=%+v err=%v", got, err)
			}
		})
	}
}

func TestNativeChanges_RedelegationUsesSameMessageOwner(t *testing.T) {
	stream := []rpc.Event{
		withMessage(nativeEvent("withdraw_rewards", "delegator", "other", "validator", "src"), "0"),
		withMessage(nativeEvent("withdraw_rewards", "delegator", "owner", "validator", "src"), "1"),
		withMessage(nativeEvent("redelegate", "source_validator", "src", "destination_validator", "dst", "completion_time", "2000-01-01T00:00:00Z", "amount", "10ualpha"), "1"),
	}
	got, err := NativeChanges(nil, []rpc.TxResult{{Events: stream}})
	if err != nil || len(got.Addresses) != 1 || got.Addresses[0] != "owner" || len(got.Validators) != 2 {
		t.Fatalf("changes=%+v err=%v", got, err)
	}
	stream = append(stream[:1], stream[2])
	if _, err := NativeChanges(nil, []rpc.TxResult{{Events: stream}}); err == nil {
		t.Fatal("borrowed owner from another message")
	}
	got, err = NativeChanges([]rpc.Event{nativeEvent("complete_redelegation", "delegator", "owner", "source_validator", "src", "destination_validator", "dst", "amount", "")}, nil)
	if err != nil || len(got.Addresses) != 1 || len(got.Validators) != 2 {
		t.Fatalf("empty completed redelegation lost: %+v %v", got, err)
	}
}

func TestNativeChanges_ValidatorCreationAndSlash(t *testing.T) {
	stream := []rpc.Event{
		nativeEvent("coin_spent", "spender", "owner", "amount", "10ualpha"),
		nativeEvent("create_validator", "validator", "operator", "amount", "10ualpha"),
	}
	got, err := NativeChanges(nil, []rpc.TxResult{{Events: stream}})
	if err != nil || len(got.Addresses) != 1 || got.Addresses[0] != "owner" || len(got.Validators) != 1 {
		t.Fatalf("self delegation: %+v %v", got, err)
	}
	got, err = NativeChanges([]rpc.Event{nativeEvent("slash", "jailed", "consensus")}, nil)
	if err != nil || got.AllExposed {
		t.Fatalf("jailing treated as a financial slash: %+v %v", got, err)
	}
	for _, amount := range []string{"0", "18446744073709551616"} {
		got, err = NativeChanges([]rpc.Event{nativeEvent("slash", "address", "consensus", "burned_coins", amount)}, nil)
		if err != nil || !got.AllExposed {
			t.Fatalf("financial slash lost: %+v %v", got, err)
		}
	}
	for _, amount := range []string{"", "-1", "1.2", "1ualpha"} {
		if _, err := NativeChanges([]rpc.Event{nativeEvent("slash", "address", "consensus", "burned_coins", amount)}, nil); err == nil {
			t.Fatalf("incomplete slash accepted: %q", amount)
		}
	}
}
