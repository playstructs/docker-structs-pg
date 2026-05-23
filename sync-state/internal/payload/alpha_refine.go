package payload

// AlphaRefine matches structs.structs.EventAlphaRefine.eventAlphaRefineDetail.
// SQL handler: cache.handle_event_alpha_refine
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2066-2089)
//
// Two ledger rows for the same primaryAddress:
//   1. `refined` debit in `ore`  (amount)
//   2. `refined` credit in `ualpha` (1_000_000 * amount → micro-units)
//
// The 1M multiplier mirrors structs.UNIT_LEGACY_FORMAT's inverse for
// ualpha (function-unit-display-format.sql:121-122). amount_p stores the
// precise micro-amount; the generated `amount` column floor-divides for
// display.
type AlphaRefine struct {
	PrimaryAddress string  `json:"primaryAddress"`
	Amount         Numeric `json:"amount"`
}
