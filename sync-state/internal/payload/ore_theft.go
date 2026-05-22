package payload

// OreTheft matches structs.structs.EventOreTheft.eventOreTheftDetail.
// SQL handler: cache.handle_event_ore_theft
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2039-2064)
//
// Two ledger rows: thief gets a `seized` credit, victim gets a `forfeited`
// debit. Counterparties cross-reference each other.
type OreTheft struct {
	ThiefPrimaryAddress  string  `json:"thiefPrimaryAddress"`
	VictimPrimaryAddress string  `json:"victimPrimaryAddress"`
	Amount               Numeric `json:"amount"`
}
