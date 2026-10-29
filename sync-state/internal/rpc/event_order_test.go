package rpc

import "testing"

func TestExecutionOrderKeepsSourceCoordinates(t *testing.T) {
	finalize := []Event{
		{Type: "upgrade"},
		{Type: "end", Attributes: []Attribute{{Key: "mode", Value: "EndBlock"}}},
		{Type: "begin", Attributes: []Attribute{{Key: "mode", Value: "BeginBlock"}}},
	}
	txs := []TxResult{{Events: []Event{{Type: "send", Attributes: []Attribute{{Key: "msg_index", Value: "1"}}}}}}
	ordered, err := OrderedEvents(finalize, txs)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"upgrade", "begin", "send", "end"} {
		if ordered[i].Event.Type != want || ordered[i].Phase != ExecutionPhase(i) {
			t.Fatalf("position %d: %+v", i, ordered[i])
		}
	}
	if ordered[3].EventIndex != 1 || ordered[3].TxIndex != -1 || ordered[2].MsgIndex != 1 || ordered[2].EventIndex != 0 {
		t.Fatalf("renumbered identity: %+v", ordered)
	}
}

func TestEventMetadataRejectsAmbiguity(t *testing.T) {
	for _, attrs := range [][]Attribute{
		{{Key: "mode", Value: "unknown"}},
		{{Key: "mode", Value: "BeginBlock"}, {Key: "mode", Value: "EndBlock"}},
		{{Key: "msg_index", Value: "0"}},
	} {
		if _, err := PositionEvent(Event{Attributes: attrs}, -1, 0); err == nil {
			t.Fatalf("accepted %+v", attrs)
		}
	}
}
