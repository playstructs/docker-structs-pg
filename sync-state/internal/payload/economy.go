package payload

// These fields retain the protobuf source names used by TypedEventToEvent.
// Presence is checked by the handlers separately from legitimate zero values.
type VaultAddress struct {
	OwnerID        string `json:"ownerId"`
	VaultAddress   string `json:"vaultAddress"`
	AccountNumber  Uint64 `json:"accountNumber"`
	AccountCreated bool   `json:"accountCreated"`
}

type OwnerReactorDiversity struct {
	OwnerID      string  `json:"ownerId"`
	Power        Natural `json:"power"`
	SquaredPower Natural `json:"squaredPower"`
	AppliedBonus Uint64  `json:"appliedBonus"`
}

type ReactorCommissionRevision struct {
	ReactorID               string `json:"reactorId"`
	SlashRevision           Uint64 `json:"slashRevision"`
	ReconciledSlashRevision Uint64 `json:"reconciledSlashRevision"`
}

type ReactorStakeCap struct {
	Active        bool   `json:"active"`
	CapBps        Uint64 `json:"capBps"`
	MinValidators Uint64 `json:"minValidators"`
}

type GuildRankPurchase struct {
	GuildID      string `json:"guildId"`
	PlayerID     string `json:"playerId"`
	PreviousRank Uint64 `json:"previousRank"`
	Rank         Uint64 `json:"rank"`
	Price        Uint64 `json:"price"`
	Denom        string `json:"denom"`
	Joined       bool   `json:"joined"`
}
