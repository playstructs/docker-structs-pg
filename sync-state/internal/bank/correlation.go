package bank

import (
	"encoding/json"
	"fmt"
	"math/big"

	"sync-state/internal/payload"
	"sync-state/internal/rpc"
)

// A group is already scoped to one execution phase, transaction and message.
// Metadata absence stays distinct from message zero. Matching still requires
// address, denomination and amount: one message can move several assets.
func transferOverlap(ev rpc.Event, group []rpc.Event) (skipSent, skipReceived bool, err error) {
	amount, denom, ok := findAmount(ev)
	if !ok || denom != "ualpha" {
		return false, false, nil
	}
	sender, recipient := findAttr(ev, "sender"), findAttr(ev, "recipient")
	for _, receipt := range group {
		switch receipt.Type {
		case "structs.structs.EventAlphaRefine":
			p, err := payload.DecodeRequired[payload.AlphaRefine](json.RawMessage(findAttr(receipt, "eventAlphaRefineDetail")), "primaryAddress", "amount")
			if err != nil {
				return false, false, err
			}
			address := p.VaultAddress
			if address == "" {
				address = p.PrimaryAddress
			}
			micro := new(big.Int).Mul(new(big.Int).SetUint64(uint64(p.Amount)), big.NewInt(1000000)).String()
			if recipient == address && amount == micro {
				for _, mint := range group {
					if mint.Type != "coinbase" || findAttr(mint, "minter") != sender {
						continue
					}
					match, err := containsCoin(mint, amount, denom)
					if err != nil {
						return false, false, err
					}
					if match {
						skipReceived = true
					}
				}
			}
		case "structs.structs.EventAlphaInfuse":
			p, err := payload.DecodeRequired[payload.AlphaRefine](json.RawMessage(findAttr(receipt, "eventAlphaInfuseDetail")), "primaryAddress", "amount")
			if err != nil {
				return false, false, err
			}
			address := p.VaultAddress
			if address == "" {
				address = p.PrimaryAddress
			}
			if sender != address || amount != p.Amount.String() {
				continue
			}
			// Reactor unbonding also emits AlphaInfuse, but never this matched
			// account-to-module transfer and burn. It is not a deposit delta.
			for _, burn := range group {
				if burn.Type != "burn" || findAttr(burn, "burner") != recipient {
					continue
				}
				match, err := containsCoin(burn, amount, denom)
				if err != nil {
					return false, false, err
				}
				if match {
					skipSent = true
				}
			}
		}
	}
	if skipSent || skipReceived {
		matches := 0
		for _, candidate := range group {
			if candidate.Type != "transfer" || findAttr(candidate, "sender") != sender || findAttr(candidate, "recipient") != recipient {
				continue
			}
			match, err := containsCoin(candidate, amount, denom)
			if err != nil {
				return false, false, err
			}
			if match {
				matches++
			}
		}
		if matches != 1 || (skipSent && skipReceived) {
			return false, false, fmt.Errorf("ambiguous overlapping transfer accounting")
		}
	}
	return skipSent, skipReceived, nil
}

func validatorSpender(group []rpc.Event, amount, denom string) (string, error) {
	spender := ""
	for _, ev := range group {
		if ev.Type != "coin_spent" {
			continue
		}
		match, err := containsCoin(ev, amount, denom)
		if err != nil {
			return "", err
		}
		if !match {
			continue
		}
		candidate := findAttr(ev, "spender")
		if candidate == "" || (spender != "" && spender != candidate) {
			return "", fmt.Errorf("ambiguous validator self-delegation spender")
		}
		spender = candidate
	}
	if spender == "" {
		return "", fmt.Errorf("validator self-delegation has no matching coin_spent")
	}
	return spender, nil
}

func redelegationOwner(group []rpc.Event, before int, validator string) (string, error) {
	if before > len(group) {
		return "", fmt.Errorf("invalid redelegation coordinate")
	}
	owner := ""
	// Staking/distribution withdraws source rewards before emitting redelegate.
	// Stop at the preceding operation; never borrow its owner's receipt.
	for i := before - 1; i >= 0; i-- {
		ev := group[i]
		if ev.Type == "redelegate" {
			break
		}
		if ev.Type != "withdraw_rewards" || findAttr(ev, "validator") != validator {
			continue
		}
		candidate := findAttr(ev, "delegator")
		if candidate == "" || (owner != "" && owner != candidate) {
			return "", fmt.Errorf("ambiguous redelegation owner")
		}
		owner = candidate
	}
	if owner == "" {
		return "", fmt.Errorf("redelegation has no preceding source reward owner")
	}
	return owner, nil
}
