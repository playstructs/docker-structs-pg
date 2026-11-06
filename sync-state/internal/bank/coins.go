package bank

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"sync-state/internal/rpc"
)

type coin struct{ amount, denom string }

// Pinned Cosmos SDK denomination grammar, with integer base units only.
var coinPattern = regexp.MustCompile(`^([0-9]+)([a-zA-Z][a-zA-Z0-9/:._-]{2,127})$`)

func parseCoins(s string) ([]coin, error) {
	if s == "" {
		return nil, nil
	}
	seen := make(map[string]bool)
	coins := make([]coin, 0, strings.Count(s, ",")+1)
	for _, part := range strings.Split(s, ",") {
		m := coinPattern.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("invalid integer coin %q", part)
		}
		if seen[m[2]] {
			return nil, fmt.Errorf("duplicate coin denomination %q", m[2])
		}
		seen[m[2]] = true
		n, ok := new(big.Int).SetString(m[1], 10)
		if !ok || n.BitLen() > 256 {
			return nil, fmt.Errorf("coin amount exceeds SDK integer range")
		}
		if n.Sign() == 0 {
			continue
		}
		coins = append(coins, coin{n.String(), m[2]})
	}
	return coins, nil
}

func eventCoins(ev rpc.Event) ([]coin, error) {
	seen := make(map[string]bool, len(ev.Attributes))
	var amount string
	for _, a := range ev.Attributes {
		if seen[a.Key] {
			return nil, fmt.Errorf("duplicate %s attribute", a.Key)
		}
		seen[a.Key] = true
		if a.Key == "amount" {
			amount = a.Value
		}
	}
	if !seen["amount"] {
		return nil, fmt.Errorf("missing amount attribute")
	}
	return parseCoins(amount)
}

func validateBankFields(ev rpc.Event) error {
	var fields []string
	switch ev.Type {
	case "transfer":
		fields = []string{"sender", "recipient"}
	case "coinbase":
		fields = []string{"minter"}
	case "burn":
		fields = []string{"burner"}
	case "delegate", "cancel_unbond", "complete_unbonding":
		fields = []string{"validator", "delegator"}
	case "unbond":
		fields = []string{"validator", "delegator", "completion_time"}
	case "redelegate":
		fields = []string{"source_validator", "destination_validator", "completion_time"}
	case "complete_redelegation":
		fields = []string{"source_validator", "destination_validator", "delegator"}
	case "create_validator":
		fields = []string{"validator"}
	}
	for _, name := range fields {
		// Testnet h=1907566 includes a successful transfer to the empty
		// address. Preserve that recipient while still requiring its key.
		value, present := lookupAttr(ev, name)
		if !present || (value == "" && name != "recipient") {
			return fmt.Errorf("missing %s attribute", name)
		}
	}
	return nil
}

func containsCoin(ev rpc.Event, amount, denom string) (bool, error) {
	coins, err := eventCoins(ev)
	if err != nil {
		return false, err
	}
	for _, c := range coins {
		if c.amount == amount && c.denom == denom {
			return true, nil
		}
	}
	return false, nil
}
