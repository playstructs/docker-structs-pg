// Package staking reads native staking claims at one verified chain height.
// Event receipts and Structs' delayed infusion cleanup are not balance sources.
package staking

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sync-state/internal/lcd"
	"sync-state/internal/payload"
)

// Limits bound a single address snapshot, not a chain-wide scan. Exceeding a
// limit is an incomplete snapshot; callers must never publish its partial sum.
type Limits struct {
	PageSize   int
	MaxPairs   int
	MaxEntries int
}

type Delegation struct {
	Validator string
	Shares    string
	Balance   string
}

type Unbonding struct {
	Validator      string
	CreationHeight int64
	CompletionTime time.Time
	InitialBalance string
	Balance        string
	ID             uint64
	HoldCount      int64
}

type Redelegation struct {
	SourceValidator      string
	DestinationValidator string
	CreationHeight       int64
	CompletionTime       time.Time
	InitialBalance       string
	Shares               string
	Balance              string
	ID                   uint64
	// No HoldCount: SDK v0.53.8 reconstructs query entries with
	// NewRedelegationEntryResponse and drops the stored hold reference count.
	// Its returned zero is not authoritative eligibility evidence.
}

type Snapshot struct {
	ChainID       string
	Address       string
	Height        int64
	Delegations   []Delegation
	Unbondings    []Unbonding
	Redelegations []Redelegation
	Bonded        string
	Unbonding     string
}

// Validators includes source-validator exposure from in-flight redelegations.
// A later source slash can affect destination stake after the source delegation
// has disappeared, so indexing only currently bonded validators is insufficient.
func (s Snapshot) Validators() []string {
	seen := map[string]bool{}
	for _, d := range s.Delegations {
		seen[d.Validator] = true
	}
	for _, u := range s.Unbondings {
		seen[u.Validator] = true
	}
	for _, r := range s.Redelegations {
		seen[r.SourceValidator] = true
		seen[r.DestinationValidator] = true
	}
	validators := make([]string, 0, len(seen))
	for validator := range seen {
		validators = append(validators, validator)
	}
	sort.Strings(validators)
	return validators
}

// Fetch returns a snapshot only after every page of all three scoped queries
// succeeds at height. Totals are exact decimal strings. Only current delegation
// balances and actual unbonding balances contribute: redelegations already
// belong to destination delegations. No wall-clock test discards held entries.
// This reader does not publish database readiness or change liquid balances.
func Fetch(ctx context.Context, client lcd.Client, chainID, address string, height int64, limits Limits) (*Snapshot, error) {
	if height <= 0 || limits.PageSize <= 0 || limits.MaxPairs <= 0 || limits.MaxEntries <= 0 {
		return nil, fmt.Errorf("native staking snapshot needs positive height and limits")
	}
	if !validAddress(address, "structs1") {
		return nil, fmt.Errorf("invalid native delegator address")
	}
	if err := client.ValidateChain(ctx, chainID); err != nil {
		return nil, err
	}
	s := &Snapshot{ChainID: chainID, Address: address, Height: height, Bonded: "0", Unbonding: "0"}
	budget := snapshotBudget{limits: limits, pairs: map[string]bool{}, ids: map[uint64]bool{}}
	bonded, unbonding := new(big.Int), new(big.Int)
	pathAddress := url.PathEscape(address)
	err := client.Pages(ctx, "/cosmos/staking/v1beta1/delegations/"+pathAddress, "delegation_responses", height, limits.PageSize, func(raw json.RawMessage) error {
		row, err := payload.DecodeRequired[delegationResponse](raw, "delegation", "balance")
		if err != nil {
			return err
		}
		d, err := payload.DecodeRequired[delegationJSON](row.Delegation, "delegator_address", "validator_address", "shares")
		if err != nil {
			return err
		}
		if err := checkPair(address, d.Delegator, d.Validator); err != nil {
			return err
		}
		if err := budget.pair("d/" + d.Validator); err != nil {
			return err
		}
		if !validShares(d.Shares) {
			return fmt.Errorf("invalid delegation shares")
		}
		coin, err := payload.DecodeRequired[coinJSON](row.Balance, "denom", "amount")
		if err != nil {
			return err
		}
		if coin.Denom != "ualpha" {
			return fmt.Errorf("unexpected staking denomination %q", coin.Denom)
		}
		amount := string(coin.Amount)
		addNatural(bonded, amount)
		s.Delegations = append(s.Delegations, Delegation{Validator: d.Validator, Shares: d.Shares, Balance: amount})
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = client.Pages(ctx, "/cosmos/staking/v1beta1/delegators/"+pathAddress+"/unbonding_delegations", "unbonding_responses", height, limits.PageSize, func(raw json.RawMessage) error {
		row, err := payload.DecodeRequired[unbondingJSON](raw, "delegator_address", "validator_address", "entries")
		if err != nil {
			return err
		}
		if err := checkPair(address, row.Delegator, row.Validator); err != nil {
			return err
		}
		if err := budget.pair("u/" + row.Validator); err != nil {
			return err
		}
		for _, rawEntry := range row.Entries {
			e, err := decodeEntry(rawEntry, height, "balance")
			if err != nil {
				return err
			}
			if err := budget.entry(uint64(e.ID)); err != nil {
				return err
			}
			balance := string(e.Balance)
			if compareNatural(balance, string(e.InitialBalance)) > 0 {
				return fmt.Errorf("unbonding balance exceeds its initial balance")
			}
			addNatural(unbonding, balance)
			s.Unbondings = append(s.Unbondings, Unbonding{Validator: row.Validator, CreationHeight: e.CreationHeight,
				CompletionTime: e.CompletionTime, InitialBalance: string(e.InitialBalance), Balance: balance,
				ID: uint64(e.ID), HoldCount: e.HoldCount})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = client.Pages(ctx, "/cosmos/staking/v1beta1/delegators/"+pathAddress+"/redelegations", "redelegation_responses", height, limits.PageSize, func(raw json.RawMessage) error {
		row, err := payload.DecodeRequired[redelegationResponse](raw, "redelegation", "entries")
		if err != nil {
			return err
		}
		r, err := payload.DecodeRequired[redelegationJSON](row.Redelegation, "delegator_address", "validator_src_address", "validator_dst_address")
		if err != nil {
			return err
		}
		if err := checkPair(address, r.Delegator, r.Source); err != nil {
			return err
		}
		if !validAddress(r.Destination, "structsvaloper1") || r.Source == r.Destination {
			return fmt.Errorf("invalid redelegation destination")
		}
		if err := budget.pair("r/" + r.Source + "/" + r.Destination); err != nil {
			return err
		}
		for _, rawEntry := range row.Entries {
			response, err := payload.DecodeRequired[redelegationEntryResponse](rawEntry, "redelegation_entry", "balance")
			if err != nil {
				return err
			}
			e, err := decodeEntry(response.Entry, height, "shares_dst")
			if err != nil {
				return err
			}
			if !validShares(e.Shares) {
				return fmt.Errorf("invalid redelegation shares")
			}
			if err := budget.entry(uint64(e.ID)); err != nil {
				return err
			}
			s.Redelegations = append(s.Redelegations, Redelegation{SourceValidator: r.Source, DestinationValidator: r.Destination,
				CreationHeight: e.CreationHeight, CompletionTime: e.CompletionTime, InitialBalance: string(e.InitialBalance),
				Shares: e.Shares, Balance: string(response.Balance), ID: uint64(e.ID)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Bonded, s.Unbonding = bonded.String(), unbonding.String()
	return s, nil
}

type delegationResponse struct {
	Delegation json.RawMessage `json:"delegation"`
	Balance    json.RawMessage `json:"balance"`
}

type delegationJSON struct {
	Delegator string `json:"delegator_address"`
	Validator string `json:"validator_address"`
	Shares    string `json:"shares"`
}

type coinJSON struct {
	Denom  string     `json:"denom"`
	Amount coinAmount `json:"amount"`
}

type unbondingJSON struct {
	Delegator string            `json:"delegator_address"`
	Validator string            `json:"validator_address"`
	Entries   []json.RawMessage `json:"entries"`
}

type redelegationResponse struct {
	Redelegation json.RawMessage   `json:"redelegation"`
	Entries      []json.RawMessage `json:"entries"`
}

type redelegationJSON struct {
	Delegator   string `json:"delegator_address"`
	Source      string `json:"validator_src_address"`
	Destination string `json:"validator_dst_address"`
}

type redelegationEntryResponse struct {
	Entry   json.RawMessage `json:"redelegation_entry"`
	Balance coinAmount      `json:"balance"`
}

type entryJSON struct {
	CreationHeight int64          `json:"creation_height,string"`
	CompletionTime time.Time      `json:"completion_time"`
	InitialBalance coinAmount     `json:"initial_balance"`
	Balance        coinAmount     `json:"balance"`
	Shares         string         `json:"shares_dst"`
	ID             payload.Uint64 `json:"unbonding_id"`
	HoldCount      int64          `json:"unbonding_on_hold_ref_count,string"`
}

// Native amounts use the SDK's 256-bit nonnegative integer domain. Aggregate
// totals use big.Int and are not narrowed back to an individual coin's range.
type coinAmount string

func (a *coinAmount) UnmarshalJSON(raw []byte) error {
	var n payload.Natural
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	v, _ := new(big.Int).SetString(string(n), 10)
	if v.BitLen() > 256 {
		return fmt.Errorf("native staking amount exceeds SDK integer range")
	}
	*a = coinAmount(n)
	return nil
}

func decodeEntry(raw json.RawMessage, height int64, valueField string) (entryJSON, error) {
	e, err := payload.DecodeRequired[entryJSON](raw, "creation_height", "completion_time", "initial_balance",
		"unbonding_id", "unbonding_on_hold_ref_count", valueField)
	if err != nil {
		return e, err
	}
	if e.CreationHeight < 0 || e.CreationHeight > height || e.CompletionTime.IsZero() || e.HoldCount < 0 {
		return e, fmt.Errorf("invalid staking entry height, time or hold count")
	}
	return e, nil
}

type snapshotBudget struct {
	limits  Limits
	pairs   map[string]bool
	ids     map[uint64]bool
	entries int
}

func (b *snapshotBudget) pair(key string) error {
	if b.pairs[key] {
		return fmt.Errorf("duplicate native staking pair %s", key)
	}
	b.pairs[key] = true
	if len(b.pairs) > b.limits.MaxPairs {
		return fmt.Errorf("native staking snapshot exceeds %d pairs", b.limits.MaxPairs)
	}
	return nil
}

func (b *snapshotBudget) entry(id uint64) error {
	b.entries++
	if b.entries > b.limits.MaxEntries {
		return fmt.Errorf("native staking snapshot exceeds %d entries", b.limits.MaxEntries)
	}
	// Zero denotes pre-ID historical entries; it cannot establish uniqueness.
	if id != 0 {
		if b.ids[id] {
			return fmt.Errorf("duplicate native staking entry ID %s", strconv.FormatUint(id, 10))
		}
		b.ids[id] = true
	}
	return nil
}

var sharesPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,18})?$`)

func validShares(value string) bool { return sharesPattern.MatchString(value) }

func checkPair(address, delegator, validator string) error {
	if delegator != address || !validAddress(validator, "structsvaloper1") {
		return fmt.Errorf("native staking response has wrong delegator or invalid validator")
	}
	return nil
}

// This is a bounded lexical check, not a substitute for the gateway's Bech32
// checksum validation. It prevents accidental path/identity confusion locally.
func validAddress(address, prefix string) bool {
	if !strings.HasPrefix(address, prefix) || len(address) <= len(prefix) || len(address) > 128 {
		return false
	}
	for _, ch := range address[len(prefix):] {
		if !strings.ContainsRune("qpzry9x8gf2tvdw0s3jn54khce6mua7l", ch) {
			return false
		}
	}
	return true
}

func addNatural(sum *big.Int, value string) {
	n, _ := new(big.Int).SetString(value, 10) // decoded by coinAmount
	sum.Add(sum, n)
}

func compareNatural(a, b string) int {
	x, _ := new(big.Int).SetString(a, 10)
	y, _ := new(big.Int).SetString(b, 10)
	return x.Cmp(y)
}
