package bank

import (
	"fmt"
	"math/big"

	"sync-state/internal/rpc"
	"sync-state/internal/staking"
)

// NativeChanges retains the complete source stream for operation correlation.
// Even an empty completion changes native entry/exposure state and must request
// a fresh query, although it contributes no financial ledger legs.
func NativeChanges(finalize []rpc.Event, txs []rpc.TxResult) (staking.Changes, error) {
	changes := staking.Changes{}
	ordered, err := rpc.OrderedEvents(finalize, txs)
	if err != nil {
		return changes, err
	}
	type scope struct {
		phase   rpc.ExecutionPhase
		tx, msg int
	}
	groups := make(map[scope][]rpc.Event)
	for _, p := range ordered {
		key := scope{p.Phase, p.TxIndex, p.MsgIndex}
		group := groups[key]
		ev := p.Event
		var address string
		switch ev.Type {
		case "delegate", "unbond", "cancel_unbond", "complete_unbonding":
			if err := validateBankFields(ev); err != nil {
				return changes, err
			}
			address = findAttr(ev, "delegator")
			changes.Validators = append(changes.Validators, findAttr(ev, "validator"))
		case "redelegate", "complete_redelegation":
			if err := validateBankFields(ev); err != nil {
				return changes, err
			}
			address = findAttr(ev, "delegator")
			if ev.Type == "redelegate" {
				address, err = redelegationOwner(group, len(group), findAttr(ev, "source_validator"))
				if err != nil {
					return changes, err
				}
			}
			changes.Validators = append(changes.Validators, findAttr(ev, "source_validator"), findAttr(ev, "destination_validator"))
		case "create_validator":
			coins, err := eventCoins(ev)
			if err != nil || len(coins) != 1 || coins[0].denom != "ualpha" {
				return changes, fmt.Errorf("native validator creation needs one ualpha self-delegation")
			}
			address, err = validatorSpender(group, coins[0].amount, coins[0].denom)
			if err != nil {
				return changes, err
			}
			changes.Validators = append(changes.Validators, findAttr(ev, "validator"))
		case "slash":
			burned := findAttr(ev, "burned_coins")
			if burned == "" && findAttr(ev, "jailed") != "" && findAttr(ev, "address") == "" {
				break // SDK jail-only notification moves no native claims.
			}
			amount, ok := new(big.Int).SetString(burned, 10)
			if !ok || amount.Sign() < 0 || amount.String() != burned || findAttr(ev, "address") == "" {
				return changes, fmt.Errorf("incomplete financial slash event")
			}
			// Slash reports a consensus address, while reactors/exposure use
			// operators. Until that mapping exists, invalidate known exposed
			// accounts conservatively; never infer an operator from the text.
			changes.AllExposed = true
		}
		if address != "" {
			changes.Addresses = append(changes.Addresses, address)
		}
		groups[key] = append(group, ev)
	}
	return changes, nil
}
