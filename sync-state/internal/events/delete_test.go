package events

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeDeleteObjectID(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"object", `{"objectId":"11-42"}`, "11-42"},
		{"bare string", `"11-42"`, "11-42"},
		{"wrapped object", `"{\"objectId\":\"11-42\"}"`, "11-42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeDeleteObjectID(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeDeleteObjectIDEmpty(t *testing.T) {
	if _, err := decodeDeleteObjectID(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected empty objectId error")
	}
}

func TestDeleteInfusionSkipsWithWarn(t *testing.T) {
	// Infusion ids are not a single-table primary key, so EventDelete
	// for type 7 must skip rather than fail the block.
	err := (deleteHandler{}).Handle(t.Context(), nil, bctx(), json.RawMessage(`{"objectId":"7-1"}`))
	if !errors.Is(err, ErrSkipWithWarn) {
		t.Fatalf("got %v, want ErrSkipWithWarn", err)
	}
}

func TestDeleteHandlerCompositeKey(t *testing.T) {
	if (deleteHandler{}).CompositeKey() != "structs.structs.EventDelete.objectId" {
		t.Fatalf("unexpected composite key %q", (deleteHandler{}).CompositeKey())
	}
}

func TestDeleteEmptyIsError(t *testing.T) {
	err := (deleteHandler{}).Handle(t.Context(), nil, bctx(), json.RawMessage(`{"objectId":""}`))
	if err == nil || errors.Is(err, ErrSkipWithWarn) {
		t.Fatalf("empty objectId should be a hard error, got %v", err)
	}
}
