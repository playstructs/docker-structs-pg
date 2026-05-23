package payload

// Agreement matches structs.structs.EventAgreement.agreement.
// SQL handler: cache.handle_event_agreement
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:92-160)
type Agreement struct {
	ID           string  `json:"id"`
	ProviderID   string  `json:"providerId"`
	AllocationID string  `json:"allocationId"`
	Capacity     JSONInt `json:"capacity"`
	StartBlock   JSONInt `json:"startBlock"`
	EndBlock     JSONInt `json:"endBlock"`
	Creator      string  `json:"creator"`
	Owner        string  `json:"owner"`
}
