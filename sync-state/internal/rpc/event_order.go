package rpc

import (
	"fmt"
	"strconv"
)

type ExecutionPhase int16

const (
	PreBlock ExecutionPhase = iota
	BeginBlock
	Transaction
	EndBlock
)

// PositionedEvent retains the original RPC coordinates. Application order
// changes at EndBlock, but financial/error identities must never be renumbered.
type PositionedEvent struct {
	Event                         Event
	Phase                         ExecutionPhase
	TxIndex, EventIndex, MsgIndex int
}

func PositionEvent(ev Event, txIndex, eventIndex int) (PositionedEvent, error) {
	p := PositionedEvent{Event: ev, TxIndex: txIndex, EventIndex: eventIndex, MsgIndex: -1}
	if txIndex >= 0 {
		p.Phase = Transaction
	}
	seen := make(map[string]bool, 2)
	for _, attr := range ev.Attributes {
		if attr.Key != "mode" && attr.Key != "msg_index" {
			continue
		}
		if seen[attr.Key] {
			return p, fmt.Errorf("duplicate event metadata %s", attr.Key)
		}
		seen[attr.Key] = true
		if attr.Key == "msg_index" {
			n, err := strconv.Atoi(attr.Value)
			if err != nil || n < 0 || txIndex < 0 {
				return p, fmt.Errorf("invalid msg_index %q", attr.Value)
			}
			p.MsgIndex = n
			continue
		}
		if txIndex >= 0 {
			return p, fmt.Errorf("block mode on transaction event")
		}
		switch attr.Value {
		case "BeginBlock":
			p.Phase = BeginBlock
		case "EndBlock":
			p.Phase = EndBlock
		default:
			return p, fmt.Errorf("unknown block event mode %q", attr.Value)
		}
	}
	return p, nil
}

func OrderedEvents(finalize []Event, txs []TxResult) ([]PositionedEvent, error) {
	var phases [4][]PositionedEvent
	appendEvent := func(ev Event, tx, index int) error {
		p, err := PositionEvent(ev, tx, index)
		if err != nil {
			return fmt.Errorf("event tx=%d index=%d: %w", tx, index, err)
		}
		phases[p.Phase] = append(phases[p.Phase], p)
		return nil
	}
	for i, ev := range finalize {
		if err := appendEvent(ev, -1, i); err != nil {
			return nil, err
		}
	}
	for tx, result := range txs {
		for i, ev := range result.Events {
			if err := appendEvent(ev, tx, i); err != nil {
				return nil, err
			}
		}
	}
	count := 0
	for _, phase := range phases {
		count += len(phase)
	}
	ordered := make([]PositionedEvent, 0, count)
	for _, phase := range phases {
		ordered = append(ordered, phase...)
	}
	return ordered, nil
}
