package bank

import (
	"testing"
	"time"

	"sync-state/internal/buffers"
	"sync-state/internal/rpc"
)

func TestParseAmountDenom(t *testing.T) {
	cases := []struct {
		in    string
		amt   string
		denom string
		ok    bool
	}{
		{"100ualpha", "100", "ualpha", true},
		{"0.5uvert", "0.5", "uvert", true},
		{"1234567890ualpha", "1234567890", "ualpha", true},
		{"42ualpha.infused", "42", "ualpha.infused", true},
		{"7u-test", "7", "u-test", true},
		// multi-coin: returns only first match (mirrors SQL regexp_matches behavior)
		{"100ualpha,500uvert", "100", "ualpha", true},
		// empty / unparseable
		{"", "", "", false},
		{"ualpha", "", "", false},
		{"abc", "", "", false},
	}
	for _, c := range cases {
		amt, denom, ok := parseAmountDenom(c.in)
		if ok != c.ok {
			t.Errorf("parseAmountDenom(%q): ok=%v want %v", c.in, ok, c.ok)
		}
		if amt != c.amt || denom != c.denom {
			t.Errorf("parseAmountDenom(%q): got (%q,%q) want (%q,%q)", c.in, amt, denom, c.amt, c.denom)
		}
	}
}

func TestIsBankEventType(t *testing.T) {
	wantYes := []string{
		"transfer", "coinbase", "burn", "delegate",
		"redelegate", "complete_redelegation",
		"unbond", "cancel_unbond", "complete_unbonding",
		"create_validator",
	}
	for _, s := range wantYes {
		if !IsBankEventType(s) {
			t.Errorf("IsBankEventType(%q) = false; want true", s)
		}
	}
	wantNo := []string{
		"", "withdraw_rewards", "coin_spent", "tx", "message",
		"structs.structs.EventPlayer",
	}
	for _, s := range wantNo {
		if IsBankEventType(s) {
			t.Errorf("IsBankEventType(%q) = true; want false", s)
		}
	}
}

func TestFindAttrAndAmount(t *testing.T) {
	ev := rpc.Event{
		Type: "transfer",
		Attributes: []rpc.Attribute{
			{Key: "recipient", Value: "structs1r"},
			{Key: "sender", Value: "structs1s"},
			{Key: "amount", Value: "100ualpha"},
		},
	}
	if got := findAttr(ev, "recipient"); got != "structs1r" {
		t.Errorf("findAttr(recipient) = %q", got)
	}
	if got := findAttr(ev, "missing"); got != "" {
		t.Errorf("findAttr(missing) = %q want \"\"", got)
	}
	amt, denom, ok := findAmount(ev)
	if !ok || amt != "100" || denom != "ualpha" {
		t.Errorf("findAmount = (%q,%q,%v); want (100,ualpha,true)", amt, denom, ok)
	}

	// Missing amount attribute → no-op
	evNoAmt := rpc.Event{Type: "transfer", Attributes: []rpc.Attribute{{Key: "sender", Value: "s"}}}
	if _, _, ok := findAmount(evNoAmt); ok {
		t.Errorf("findAmount missing: ok=true want false")
	}

	// Empty amount value → no-op
	evEmpty := rpc.Event{Type: "transfer", Attributes: []rpc.Attribute{{Key: "amount", Value: ""}}}
	if _, _, ok := findAmount(evEmpty); ok {
		t.Errorf("findAmount empty: ok=true want false")
	}
}

func TestFindInGroup(t *testing.T) {
	group := []rpc.Event{
		{Type: "redelegate", Attributes: []rpc.Attribute{{Key: "amount", Value: "10ualpha"}}},
		{Type: "withdraw_rewards", Attributes: []rpc.Attribute{
			{Key: "delegator", Value: "structs1deleg"},
			{Key: "validator", Value: "structsvaloper1abc"},
		}},
		{Type: "coin_spent", Attributes: []rpc.Attribute{{Key: "spender", Value: "structs1sp"}}},
	}
	if got := findInGroup(group, "withdraw_rewards", "delegator"); got != "structs1deleg" {
		t.Errorf("findInGroup(withdraw_rewards, delegator) = %q", got)
	}
	if got := findInGroup(group, "coin_spent", "spender"); got != "structs1sp" {
		t.Errorf("findInGroup(coin_spent, spender) = %q", got)
	}
	if got := findInGroup(group, "missing", "delegator"); got != "" {
		t.Errorf("findInGroup(missing) = %q want \"\"", got)
	}
}

func TestCaptureFiltering(t *testing.T) {
	finalize := []rpc.Event{
		{Type: "transfer", Attributes: []rpc.Attribute{{Key: "amount", Value: "1ualpha"}}},
		{Type: "structs.structs.EventTime", Attributes: []rpc.Attribute{{Key: "eventTimeDetail", Value: "{}"}}},
		{Type: "coinbase", Attributes: []rpc.Attribute{{Key: "amount", Value: "2ualpha"}}},
	}
	txr := []rpc.TxResult{{
		Events: []rpc.Event{
			{Type: "message", Attributes: nil},
			{Type: "transfer", Attributes: []rpc.Attribute{{Key: "amount", Value: "3ualpha"}}},
		},
	}}
	buf := Capture(42, finalize, txr)
	if buf.Height != 42 {
		t.Errorf("height = %d", buf.Height)
	}
	if len(buf.Finalize) != 2 {
		t.Errorf("finalize: got %d want 2 (transfer+coinbase, skip EventTime)", len(buf.Finalize))
	}
	if len(buf.Txs) != 1 {
		t.Errorf("txs groups: got %d want 1", len(buf.Txs))
	}
	if len(buf.Txs[0]) != 1 {
		t.Errorf("tx[0]: got %d want 1 (transfer; skip message)", len(buf.Txs[0]))
	}
}

func TestIsStructInfusionTx(t *testing.T) {
	structInfusion := []rpc.Event{
		{Type: "message", Attributes: []rpc.Attribute{
			{Key: "action", Value: "/structs.structs.MsgStructGeneratorInfuse"},
		}},
		{Type: "structs.structs.EventInfusion", Attributes: []rpc.Attribute{
			{Key: "infusion", Value: `{"destinationType":"struct","destinationId":"5-1916","fuel":"1000000"}`},
		}},
	}
	if !isStructInfusionTx(structInfusion) {
		t.Error("struct infusion group: want true")
	}

	reactorInfusion := []rpc.Event{
		{Type: "message", Attributes: []rpc.Attribute{
			{Key: "action", Value: "/structs.structs.MsgReactorInfuse"},
		}},
		{Type: "structs.structs.EventInfusion", Attributes: []rpc.Attribute{
			{Key: "infusion", Value: `{"destinationType":"reactor","destinationId":"3-1","fuel":"3000000"}`},
		}},
	}
	if isStructInfusionTx(reactorInfusion) {
		t.Error("reactor infusion group: want false")
	}

	if isStructInfusionTx([]rpc.Event{{
		Type: "transfer",
		Attributes: []rpc.Attribute{
			{Key: "sender", Value: "a"},
			{Key: "recipient", Value: "b"},
			{Key: "amount", Value: "1ualpha"},
		},
	}}) {
		t.Error("plain transfer group: want false")
	}
}

func transferEventTest(sender, recipient, amount string) rpc.Event {
	return rpc.Event{Type: "transfer", Attributes: []rpc.Attribute{
		{Key: "recipient", Value: recipient},
		{Key: "sender", Value: sender},
		{Key: "amount", Value: amount},
	}}
}

func TestHandleTransfer_StructInfusionSkipsSent(t *testing.T) {
	player := "structs1player"
	pool := "structs1pool"
	group := []rpc.Event{
		{Type: "structs.structs.EventInfusion", Attributes: []rpc.Attribute{
			{Key: "infusion", Value: `{"destinationType":"struct","destinationId":"5-1","fuel":"1000000"}`},
		}},
		transferEventTest(player, pool, "1000000ualpha"),
	}
	buf := buffers.New()
	tm := time.Date(2026, 6, 2, 22, 0, 0, 0, time.UTC)
	if err := handleTransfer(buf, 977521, tm, group[1], group); err != nil {
		t.Fatalf("handleTransfer: %v", err)
	}
	if len(buf.Ledger) != 1 {
		t.Fatalf("ledger rows = %d want 1 (received only)", len(buf.Ledger))
	}
	row := buf.Ledger[0]
	if row.Action != "received" || row.Direction != "credit" || row.Address != pool || row.Counterparty != player {
		t.Errorf("got %+v; want received credit on pool from player", row)
	}
}

func TestHandleTransfer_NormalTransferWritesBoth(t *testing.T) {
	buf := buffers.New()
	tm := time.Date(2026, 6, 2, 22, 0, 0, 0, time.UTC)
	ev := transferEventTest("structs1sender", "structs1recipient", "50ualpha")
	if err := handleTransfer(buf, 1000, tm, ev, []rpc.Event{ev}); err != nil {
		t.Fatalf("handleTransfer: %v", err)
	}
	if len(buf.Ledger) != 2 {
		t.Fatalf("ledger rows = %d want 2", len(buf.Ledger))
	}
}

func TestIsAlphaRefineTx(t *testing.T) {
	refine := []rpc.Event{
		{Type: "message", Attributes: []rpc.Attribute{
			{Key: "action", Value: "/structs.structs.MsgStructOreRefineryComplete"},
		}},
		{Type: "structs.structs.EventAlphaRefine", Attributes: []rpc.Attribute{
			{Key: "eventAlphaRefineDetail", Value: `{"primaryAddress":"structs1p","amount":"1"}`},
		}},
	}
	if !isAlphaRefineTx(refine) {
		t.Error("refine group: want true")
	}
	if isAlphaRefineTx([]rpc.Event{transferEventTest("a", "b", "1ualpha")}) {
		t.Error("plain transfer group: want false")
	}
}

func TestHandleTransfer_AlphaRefineSkipsReceived(t *testing.T) {
	player := "structs1refiner"
	pool := "structs1pool"
	group := []rpc.Event{
		{Type: "structs.structs.EventAlphaRefine", Attributes: []rpc.Attribute{
			{Key: "eventAlphaRefineDetail", Value: `{"primaryAddress":"structs1refiner","amount":"1"}`},
		}},
		transferEventTest(pool, player, "1000000ualpha"),
	}
	buf := buffers.New()
	tm := time.Date(2026, 4, 3, 3, 0, 0, 0, time.UTC)
	if err := handleTransfer(buf, 107304, tm, group[1], group); err != nil {
		t.Fatalf("handleTransfer: %v", err)
	}
	if len(buf.Ledger) != 1 {
		t.Fatalf("ledger rows = %d want 1 (sent only)", len(buf.Ledger))
	}
	row := buf.Ledger[0]
	if row.Action != "sent" || row.Direction != "debit" || row.Address != pool || row.Counterparty != player {
		t.Errorf("got %+v; want sent debit on pool to player", row)
	}
}
