package staking

import (
	"context"
	"fmt"
	"math/big"
	"sort"

	"github.com/jackc/pgx/v5"
)

type positionTotal struct {
	validator string
	bonded    *big.Int
	unbonding *big.Int
	entries   int
}

// Group the already complete address query. Never infer a validator balance by
// spreading an address total across its infusions, or count redelegations twice.
func positionTotals(s *Snapshot) ([]positionTotal, error) {
	byValidator := map[string]*positionTotal{}
	get := func(validator string) *positionTotal {
		if byValidator[validator] == nil {
			byValidator[validator] = &positionTotal{validator: validator, bonded: new(big.Int), unbonding: new(big.Int)}
		}
		return byValidator[validator]
	}
	seen := map[string]bool{}
	for _, d := range s.Delegations {
		if seen[d.Validator] || !naturalTotal(d.Balance) {
			return nil, fmt.Errorf("incomplete or duplicate native delegation position")
		}
		seen[d.Validator] = true
		get(d.Validator).bonded.SetString(d.Balance, 10)
	}
	for _, u := range s.Unbondings {
		if !naturalTotal(u.Balance) {
			return nil, fmt.Errorf("incomplete native unbonding position")
		}
		value, _ := new(big.Int).SetString(u.Balance, 10)
		p := get(u.Validator)
		p.unbonding.Add(p.unbonding, value)
		p.entries++ // A held zero-balance entry still consumes a staking slot.
	}
	result := make([]positionTotal, 0, len(byValidator))
	bonded, unbonding := new(big.Int), new(big.Int)
	for _, p := range byValidator {
		bonded.Add(bonded, p.bonded)
		unbonding.Add(unbonding, p.unbonding)
		result = append(result, *p)
	}
	if bonded.String() != s.Bonded || unbonding.String() != s.Unbonding {
		return nil, fmt.Errorf("native position totals disagree with complete address totals")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].validator < result[j].validator })
	return result, nil
}

func replacePositions(ctx context.Context, tx pgx.Tx, s *Snapshot, positions []positionTotal) error {
	if _, err := tx.Exec(ctx, `DELETE FROM sync_state.native_staking_position WHERE chain_id=$1 AND address=$2`, s.ChainID, s.Address); err != nil {
		return err
	}
	if len(positions) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(positions))
	for _, p := range positions {
		rows = append(rows, []any{s.ChainID, s.Address, p.validator, p.bonded.String(), p.unbonding.String(), p.entries})
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"sync_state", "native_staking_position"},
		[]string{"chain_id", "address", "validator", "bonded", "unbonding", "unbonding_entries"}, pgx.CopyFromRows(rows))
	return err
}
