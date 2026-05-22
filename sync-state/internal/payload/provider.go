package payload

// Provider matches structs.structs.EventProvider.provider.
// SQL handler: cache.handle_event_provider
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:606-700)
type Provider struct {
	ID                          string         `json:"id"`
	Index                       JSONInt        `json:"index"`
	SubstationID                string         `json:"substationId"`
	Rate                        ProviderRate   `json:"rate"`
	AccessPolicy                string         `json:"accessPolicy"`
	CapacityMinimum             Numeric        `json:"capacityMinimum"`
	CapacityMaximum             Numeric        `json:"capacityMaximum"`
	DurationMinimum             Numeric        `json:"durationMinimum"`
	DurationMaximum             Numeric        `json:"durationMaximum"`
	ProviderCancellationPenalty Numeric        `json:"providerCancellationPenalty"`
	ConsumerCancellationPenalty Numeric        `json:"consumerCancellationPenalty"`
	Creator                     string         `json:"creator"`
	Owner                       string         `json:"owner"`
}

// ProviderRate is the nested Coin-shaped {amount, denom} the chain emits.
// Mirrors the SQL handler's `(v.rate->>'amount')::NUMERIC, v.rate->>'denom'`
// destructuring into rate_amount / rate_denom.
type ProviderRate struct {
	Amount Numeric `json:"amount"`
	Denom  string  `json:"denom"`
}
