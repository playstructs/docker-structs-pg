package bank

import (
	"context"
	"testing"
	"time"

	"sync-state/internal/buffers"
	"sync-state/internal/rpc"
)

func TestCoins_ExactSetsAndInvalidAmounts(t *testing.T) {
	coins, err := parseCoins("18446744073709551615factory/structs1issuer/token_name,2ibc/ABCDEF,0ualpha")
	if err != nil || len(coins) != 2 || coins[0].amount != "18446744073709551615" || coins[0].denom != "factory/structs1issuer/token_name" {
		t.Fatalf("coins=%+v err=%v", coins, err)
	}
	for _, bad := range []string{"1.5ualpha", "-1ualpha", "+1ualpha", "1ualpha,", "1ualpha,2ualpha", "1e3ualpha ", "1ualpha garbage", "1u", "1ualpha/??"} {
		if _, err := parseCoins(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if coins, err := parseCoins(""); err != nil || len(coins) != 0 {
		t.Fatal("empty SDK coin set rejected")
	}
	if _, err := eventCoins(rpc.Event{}); err == nil {
		t.Fatal("missing amount treated as empty set")
	}
}

func withMessage(ev rpc.Event, msg string) rpc.Event {
	ev.Attributes = append(ev.Attributes, rpc.Attribute{Key: "msg_index", Value: msg})
	return ev
}

func TestBank_MixedMessagesOnlySuppressMatchedFunding(t *testing.T) {
	funding := []rpc.Event{
		transferEventTest("vault", "module", "10ualpha"),
		{Type: "burn", Attributes: []rpc.Attribute{{Key: "burner", Value: "module"}, {Key: "amount", Value: "10ualpha"}}},
		{Type: "structs.structs.EventAlphaInfuse", Attributes: []rpc.Attribute{{Key: "eventAlphaInfuseDetail", Value: `{"primaryAddress":"signer","vaultAddress":"vault","amount":"10"}`}}},
		transferEventTest("vault", "other", "10ualpha,3factory/issuer/coin"),
	}
	for i := range funding {
		funding[i] = withMessage(funding[i], "0")
	}
	// Neither an ante event nor message 1 inherits message 0's receipt.
	stream := []rpc.Event{transferEventTest("vault", "fee", "1ualpha")}
	stream = append(stream, funding...)
	stream = append(stream, withMessage(transferEventTest("vault", "module", "10ualpha"), "1"))
	buf := buffers.New()
	if err := ProcessBlock(context.Background(), nil, buf, 1, time.Now(), "test", nil, []rpc.TxResult{{Events: stream}}); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{}
	for _, row := range buf.Ledger {
		if row.Address == "vault" && row.Direction == "debit" {
			sent[row.Counterparty+"/"+row.Denom] = row.AmountP
		}
	}
	if len(sent) != 4 || sent["fee/ualpha"] != "1" || sent["other/ualpha"] != "10" || sent["module/ualpha"] != "10" || sent["other/factory/issuer/coin"] != "3" {
		t.Fatalf("unrelated transfer suppressed: %+v", sent)
	}
	for _, row := range buf.Ledger {
		if row.MsgIndex != -1 {
			t.Fatal("bank persisted message index changed legacy identity")
		}
	}
}

func TestBank_RefinementOnlySuppressesMintedVaultCredit(t *testing.T) {
	stream := []rpc.Event{
		{Type: "coinbase", Attributes: []rpc.Attribute{{Key: "minter", Value: "module"}, {Key: "amount", Value: "1000000ualpha"}}},
		transferEventTest("module", "vault", "1000000ualpha"),
		{Type: "structs.structs.EventAlphaRefine", Attributes: []rpc.Attribute{{Key: "eventAlphaRefineDetail", Value: `{"primaryAddress":"signer","vaultAddress":"vault","amount":"1"}`}}},
		transferEventTest("unrelated", "vault", "1000000ualpha"),
	}
	for i := range stream {
		stream[i] = withMessage(stream[i], "0")
	}
	buf := buffers.New()
	if err := ProcessBlock(context.Background(), nil, buf, 1, time.Now(), "test", nil, []rpc.TxResult{{Events: stream}}); err != nil {
		t.Fatal(err)
	}
	credits := 0
	for _, row := range buf.Ledger {
		if row.Address == "vault" && row.Direction == "credit" {
			credits++
			if row.Counterparty != "unrelated" {
				t.Fatal("refinement credited twice")
			}
		}
	}
	if credits != 1 {
		t.Fatalf("unrelated credit lost: %d", credits)
	}
}

func TestBank_RedelegationOwnersFollowActualOperations(t *testing.T) {
	var stream []rpc.Event
	for _, owner := range []string{"owner-a", "owner-b"} {
		stream = append(stream,
			rpc.Event{Type: "withdraw_rewards", Attributes: []rpc.Attribute{{Key: "validator", Value: "source"}, {Key: "delegator", Value: owner}}},
			rpc.Event{Type: "redelegate", Attributes: []rpc.Attribute{{Key: "source_validator", Value: "source"}, {Key: "destination_validator", Value: "dest"}, {Key: "amount", Value: "10ualpha"}, {Key: "completion_time", Value: "2026-10-29T00:00:00Z"}}},
		)
	}
	for i := range stream {
		stream[i] = withMessage(stream[i], "0")
	}
	buf := buffers.New()
	if err := ProcessBlock(context.Background(), nil, buf, 1, time.Now(), "test", nil, []rpc.TxResult{{Events: stream}}); err != nil {
		t.Fatal(err)
	}
	if len(buf.Defusion) != 2 || buf.Defusion[0].DelegatorAddress != "owner-a" || buf.Defusion[1].DelegatorAddress != "owner-b" {
		t.Fatalf("owners=%+v", buf.Defusion)
	}
}

func TestBank_EmptyCompletionsHaveNoFinancialLeg(t *testing.T) {
	for _, kind := range []string{"complete_unbonding", "complete_redelegation"} {
		ev := rpc.Event{Type: kind, Attributes: []rpc.Attribute{{Key: "amount", Value: ""}, {Key: "validator", Value: "source"}, {Key: "source_validator", Value: "source"}, {Key: "destination_validator", Value: "dest"}, {Key: "delegator", Value: "owner"}}}
		buf := buffers.New()
		if err := ProcessBlock(context.Background(), nil, buf, 1, time.Now(), "test", []rpc.Event{ev}, nil); err != nil {
			t.Fatal(err)
		}
		if len(buf.Ledger) != 0 || len(buf.Defusion) != 0 {
			t.Fatal("empty completion invented value or a pending operation")
		}
	}
}
