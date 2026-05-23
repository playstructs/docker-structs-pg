// Unit tests for the Phase 4 attribute-id parsers. These exercise only
// pure Go logic — no DB. The integration tests (handlers_phase4_integration_test.go)
// cover the SQL paths.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestParseGridAttributeID(t *testing.T) {
	cases := []struct {
		in                    string
		wantSub, wantT, wantI int
		wantErr               bool
	}{
		{"0-5-42", 0, 5, 42, false},
		{"7-9-1", 7, 9, 1, false},
		{"14-2-0", 14, 2, 0, false},
		{"99-11-1000", 99, 11, 1000, false}, // out-of-range subIdx is fine at parse time
		{"", 0, 0, 0, true},
		{"0", 0, 0, 0, true},
		{"0-5", 0, 0, 0, true},
		{"a-5-1", 0, 0, 0, true},
		{"0-b-1", 0, 0, 0, true},
		{"0-5-c", 0, 0, 0, true},
		{"0--1", 0, 0, 0, true}, // empty middle part
	}
	for _, c := range cases {
		s, ot, oi, err := parseGridAttributeID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if s != c.wantSub || ot != c.wantT || oi != c.wantI {
			t.Errorf("%q: got (%d,%d,%d), want (%d,%d,%d)", c.in, s, ot, oi, c.wantSub, c.wantT, c.wantI)
		}
	}
}

// TestGridHandler_MalformedAttributeReturnsWarn asserts gridHandler returns
// ErrSkipWithWarn for malformed attributeIds (empty, missing parts) so the
// router lands them in handler_error_log with severity='warn' instead of
// blowing up the per-block tx. Non-numeric segments stay fatal. This is a
// pure-Go test — payload decode + parseGridAttributeID fire before any SQL.
func TestGridHandler_MalformedAttributeReturnsWarn(t *testing.T) {
	warnCases := []string{"", "2-", "0-", "-3-4", "0--1"}
	for _, id := range warnCases {
		raw, _ := json.Marshal(map[string]any{"attributeId": id, "value": "100"})
		err := (gridHandler{}).Handle(context.Background(), nil, BlockContext{}, raw)
		if err == nil {
			t.Errorf("%q: expected error", id)
			continue
		}
		if !errors.Is(err, ErrSkipWithWarn) {
			t.Errorf("%q: want ErrSkipWithWarn, got %v", id, err)
		}
	}

	// Non-numeric is a real chain bug — must NOT be downgraded to warn.
	raw, _ := json.Marshal(map[string]any{"attributeId": "a-5-1", "value": "100"})
	err := (gridHandler{}).Handle(context.Background(), nil, BlockContext{}, raw)
	if err == nil {
		t.Fatalf("non-numeric attributeId: expected error")
	}
	if errors.Is(err, ErrSkipWithWarn) {
		t.Errorf("non-numeric attributeId must NOT be ErrSkipWithWarn; got %v", err)
	}
}

// TestParseGridAttributeID_MissingPartsSentinel asserts that the missing-part
// cases return errGridAttrMissingParts (so the grid handler can downgrade
// to ErrSkipWithWarn rather than fail the per-block tx). Non-numeric cases
// must NOT carry the sentinel — those stay severity='error' so the operator
// runbook fires.
func TestParseGridAttributeID_MissingPartsSentinel(t *testing.T) {
	missing := []string{"", "0", "0-5", "2-", "-3-4", "0--1"}
	for _, in := range missing {
		_, _, _, err := parseGridAttributeID(in)
		if err == nil {
			t.Errorf("%q: expected error", in)
			continue
		}
		if !errors.Is(err, errGridAttrMissingParts) {
			t.Errorf("%q: want errGridAttrMissingParts, got %v", in, err)
		}
	}

	nonNumeric := []string{"a-5-1", "0-b-1", "0-5-c"}
	for _, in := range nonNumeric {
		_, _, _, err := parseGridAttributeID(in)
		if err == nil {
			t.Errorf("%q: expected error", in)
			continue
		}
		if errors.Is(err, errGridAttrMissingParts) {
			t.Errorf("%q: non-numeric should NOT carry missing-parts sentinel, got %v", in, err)
		}
	}
}

func TestParseStructAttributeID(t *testing.T) {
	cases := []struct {
		in                              string
		wantAttr, wantT, wantI, wantSub int
		wantErr                         bool
	}{
		{"0-5-42", 0, 5, 42, 0, false},   // health, no explicit subIndex
		{"1-5-42-3", 1, 5, 42, 3, false}, // status, explicit subIndex=3
		{"6-5-1-0", 6, 5, 1, 0, false},   // typeCount, explicit zero
		{"0-5-42-", 0, 5, 42, 0, false},  // trailing dash → empty subIndex → 0
		{"", 0, 0, 0, 0, true},
		{"a-5-1", 0, 0, 0, 0, true},
		{"0-5-1-foo", 0, 0, 0, 0, true},
	}
	for _, c := range cases {
		a, ot, oi, su, err := parseStructAttributeID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if a != c.wantAttr || ot != c.wantT || oi != c.wantI || su != c.wantSub {
			t.Errorf("%q: got (%d,%d,%d,%d), want (%d,%d,%d,%d)", c.in, a, ot, oi, su, c.wantAttr, c.wantT, c.wantI, c.wantSub)
		}
	}
}

func TestParsePlanetAttributeID(t *testing.T) {
	cases := []struct {
		in                     string
		wantAttr, wantT, wantI int
		wantErr                bool
	}{
		{"0-2-1", 0, 2, 1, false},
		{"10-2-99", 10, 2, 99, false},
		{"", 0, 0, 0, true},
		{"0", 0, 0, 0, true},
		{"a-2-1", 0, 0, 0, true},
	}
	for _, c := range cases {
		a, ot, oi, err := parsePlanetAttributeID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if a != c.wantAttr || ot != c.wantT || oi != c.wantI {
			t.Errorf("%q: got (%d,%d,%d), want (%d,%d,%d)", c.in, a, ot, oi, c.wantAttr, c.wantT, c.wantI)
		}
	}
}

func TestGridLabelFor(t *testing.T) {
	// Spot-check every defined index + out-of-range cases.
	for i, want := range []string{"ore", "fuel", "capacity", "load",
		"structsLoad", "power", "connectionCapacity", "connectionCount",
		"allocationPointerStart", "allocationPointerEnd",
		"proxyNonce", "lastAction", "nonce", "ready", "checkpointBlock"} {
		if got := gridLabelFor(i); got != want {
			t.Errorf("gridLabelFor(%d) = %q want %q", i, got, want)
		}
	}
	if got := gridLabelFor(-1); got != "" {
		t.Errorf("gridLabelFor(-1) = %q want empty", got)
	}
	if got := gridLabelFor(15); got != "" {
		t.Errorf("gridLabelFor(15) = %q want empty", got)
	}
}

func TestStructAttrLabelFor(t *testing.T) {
	for i, want := range []string{"health", "status", "blockStartBuild",
		"blockStartOreMine", "blockStartOreRefine", "protectedStructIndex",
		"typeCount"} {
		if got := structAttrLabelFor(i); got != want {
			t.Errorf("structAttrLabelFor(%d) = %q want %q", i, got, want)
		}
	}
	if got := structAttrLabelFor(7); got != "" {
		t.Errorf("structAttrLabelFor(7) = %q want empty", got)
	}
}

func TestPlanetAttrLabelFor(t *testing.T) {
	for i, want := range []string{
		"planetaryShield",
		"repairNetworkQuantity",
		"defensiveCannonQuantity",
		"coordinatedGlobalShieldNetworkQuantity",
		"lowOrbitBallisticsInterceptorNetworkQuantity",
		"advancedLowOrbitBallisticsInterceptorNetworkQuantity",
		"lowOrbitBallisticsInterceptorNetworkSuccessRateNumerator",
		"lowOrbitBallisticsInterceptorNetworkSuccessRateDenominator",
		"orbitalJammingStationQuantity",
		"advancedOrbitalJammingStationQuantity",
		"blockStartRaid",
	} {
		if got := planetAttrLabelFor(i); got != want {
			t.Errorf("planetAttrLabelFor(%d) = %q want %q", i, got, want)
		}
	}
	if got := planetAttrLabelFor(11); got != "" {
		t.Errorf("planetAttrLabelFor(11) = %q want empty", got)
	}
}
