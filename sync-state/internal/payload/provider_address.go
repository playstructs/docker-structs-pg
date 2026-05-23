package payload

// ProviderAddress matches structs.structs.EventProviderAddress.eventProviderAddressDetail.
// SQL handler: cache.handle_event_provider_address
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2153-2208)
//
// Writes FOUR address_tag rows (2 addresses × 2 labels each):
//   - collateralPool: Type='Provider Collateral Pool',  ProviderId=providerId
//   - earningPool:    Type='Provider Earning Pool',     ProviderId=providerId
type ProviderAddress struct {
	CollateralPool string `json:"collateralPool"`
	ProviderID     string `json:"providerId"`
	EarningPool    string `json:"earningPool"`
}
