// Package genesis ports docker-structsd's `scripts/indexer-insert-genesis.sh`
// into Go and folds it into sync-state.
//
// What it does:
//
//  1. Loads the chain's genesis JSON (from CometBFT /genesis[/_chunked]
//     by default, or from a local file via -genesis-file).
//  2. Parses the four sections the indexer cares about:
//     - app_state.bank.balances              → structs.ledger (credit/genesis, per-coin denom)
//     - app_state.staking.delegations        → structs.ledger × 2 (delegator+validator, ualpha.infused)
//     - app_state.staking.unbonding_delegations → structs.ledger × 2 (defusing, ualpha.defusing)
//     - app_state.structs.gridList (attr 0-1-*) → structs.ledger (ore credits)
//  3. Wraps the four section inserts in a single transaction with
//     savepoint-per-section so a malformed entry in one section doesn't
//     blow up the others.
//  4. Records a sync_state.genesis_log row so subsequent ingest runs
//     know not to re-apply (and so the verify check can FAIL loudly
//     when cursor>0 but genesis was never loaded — which is what
//     produces the negative-balance false alarms seen pre-port).
//
// Replay safety: Apply() first deletes every structs.ledger row with
// action='genesis' and then re-inserts. Re-running init-genesis is
// always safe; the genesis_log row gets ON CONFLICT-replaced too. The
// table has no chain_id column (single-chain by design) so the wipe is
// global per the same convention the shell script uses.
package genesis

import (
	"encoding/json"
	"fmt"
	"math/big"
	"time"
)

// Document is the slice of the genesis JSON that this package cares
// about. Unknown fields are tolerated (encoding/json default) so a
// future genesis-format change in unrelated subsystems doesn't break
// us.
type Document struct {
	GenesisTime time.Time `json:"genesis_time"`
	ChainID     string    `json:"chain_id"`
	AppState    AppState  `json:"app_state"`
}

// AppState mirrors `app_state.{bank,staking,structs}` exactly enough to
// reproduce the four-section import.
type AppState struct {
	Bank    BankState    `json:"bank"`
	Staking StakingState `json:"staking"`
	Structs StructsState `json:"structs"`
}

type BankState struct {
	Balances []BankBalance `json:"balances"`
}

type BankBalance struct {
	Address string `json:"address"`
	Coins   []Coin `json:"coins"`
}

type Coin struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"` // decimal string; can exceed int64 (megaualpha)
}

type StakingState struct {
	Validators           []Validator           `json:"validators"`
	Delegations          []Delegation          `json:"delegations"`
	UnbondingDelegations []UnbondingDelegation `json:"unbonding_delegations"`
}

type Validator struct {
	OperatorAddress string `json:"operator_address"`
	Tokens          string `json:"tokens"`           // integer-ish, decimal string
	DelegatorShares string `json:"delegator_shares"` // can be fractional ("1234567.890000000000000000")
}

type Delegation struct {
	DelegatorAddress string `json:"delegator_address"`
	ValidatorAddress string `json:"validator_address"`
	Shares           string `json:"shares"`
}

type UnbondingDelegation struct {
	DelegatorAddress string                `json:"delegator_address"`
	ValidatorAddress string                `json:"validator_address"`
	Entries          []UnbondingEntry      `json:"entries"`
}

type UnbondingEntry struct {
	Balance string `json:"balance"`
}

type StructsState struct {
	PlayerList []PlayerRow `json:"playerList"`
	GridList   []GridRow   `json:"gridList"`
}

type PlayerRow struct {
	Index          string `json:"index"`
	PrimaryAddress string `json:"primaryAddress"`
}

type GridRow struct {
	AttributeID string `json:"attributeId"`
	Value       string `json:"value"`
}

// Parse decodes the raw genesis JSON bytes into a Document. Surfaces a
// helpful error including the byte length so a totally-empty body
// (sometimes seen from misconfigured proxies) is obvious in logs.
func Parse(raw []byte) (*Document, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("genesis: empty payload")
	}
	var d Document
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("genesis: decode (len=%d): %w", len(raw), err)
	}
	if d.ChainID == "" {
		return nil, fmt.Errorf("genesis: missing chain_id (parsed %d bytes)", len(raw))
	}
	if d.GenesisTime.IsZero() {
		return nil, fmt.Errorf("genesis: missing or invalid genesis_time (chain=%s)", d.ChainID)
	}
	return &d, nil
}

// delegationAmount reproduces the shell script's amount math:
//
//	(shares * vTokens / vShares) | floor
//
// All three inputs are decimal strings that can exceed int64 — modern
// Cosmos chains carry token amounts in micro-units with 18-decimal-place
// shares. We use math/big.Rat for the intermediate to preserve
// precision, then Floor to int. Returns the canonical decimal string
// representation Postgres NUMERIC can absorb directly.
//
// Returns "0" when vShares is zero (matches the shell script's defensive
// branch); returns an error only when the inputs aren't parseable
// numbers — which would indicate a malformed genesis we should refuse
// to import, not silently round to zero.
func delegationAmount(shares, vTokens, vShares string) (string, error) {
	s, ok := new(big.Rat).SetString(shares)
	if !ok {
		return "", fmt.Errorf("delegationAmount: shares not a number: %q", shares)
	}
	vt, ok := new(big.Rat).SetString(vTokens)
	if !ok {
		return "", fmt.Errorf("delegationAmount: validator tokens not a number: %q", vTokens)
	}
	vs, ok := new(big.Rat).SetString(vShares)
	if !ok {
		return "", fmt.Errorf("delegationAmount: validator shares not a number: %q", vShares)
	}
	if vs.Sign() <= 0 {
		return "0", nil
	}
	// (s * vt) / vs  ->  floor  ->  decimal string
	num := new(big.Rat).Mul(s, vt)
	num.Quo(num, vs)
	// big.Rat doesn't expose Floor; do it via Num/Denom integer division
	// (rounds toward zero, which equals Floor for non-negative values —
	// all delegation amounts are >= 0 by construction).
	q := new(big.Int).Quo(num.Num(), num.Denom())
	return q.String(), nil
}
