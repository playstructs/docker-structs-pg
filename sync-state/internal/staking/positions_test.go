package staking

import "testing"

func TestPositionTotals_ExactPerValidatorAndNoRedelegationAsset(t *testing.T) {
	s := &Snapshot{Bonded: "18446744073709551619", Unbonding: "7",
		Delegations: []Delegation{{Validator: validatorB, Balance: "3"}, {Validator: validatorA, Balance: "18446744073709551616"}},
		Unbondings: []Unbonding{{Validator: validatorB, Balance: "2"}, {Validator: validatorB, Balance: "5"},
			{Validator: validatorC, Balance: "0", HoldCount: 1}},
		Redelegations: []Redelegation{{SourceValidator: validatorC, DestinationValidator: validatorA, Balance: "100"}},
	}
	got, err := positionTotals(s)
	if err != nil || len(got) != 3 {
		t.Fatalf("positions: %v %v", got, err)
	}
	want := []struct {
		validator, bonded, unbonding string
		entries                      int
	}{{validatorA, "18446744073709551616", "0", 0}, {validatorB, "3", "7", 2}, {validatorC, "0", "0", 1}}
	for i, p := range got {
		if p.validator != want[i].validator || p.bonded.String() != want[i].bonded || p.unbonding.String() != want[i].unbonding || p.entries != want[i].entries {
			t.Fatalf("position %d: %+v; want %+v", i, p, want[i])
		}
	}
}

func TestPositionTotals_RejectsPartialOrInvalidEvidence(t *testing.T) {
	for name, s := range map[string]*Snapshot{
		"missing delegation": {Bonded: "1", Unbonding: "0"},
		"missing unbonding":  {Bonded: "0", Unbonding: "1"},
		"duplicate validator": {Bonded: "2", Unbonding: "0", Delegations: []Delegation{
			{Validator: validatorA, Balance: "1"}, {Validator: validatorA, Balance: "1"}}},
		"fractional": {Bonded: "1", Unbonding: "0", Delegations: []Delegation{{Validator: validatorA, Balance: "1.0"}}},
		"negative":   {Bonded: "0", Unbonding: "0", Unbondings: []Unbonding{{Validator: validatorA, Balance: "-1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := positionTotals(s); err == nil {
				t.Fatal("accepted incomplete or malformed positions")
			}
		})
	}
}
