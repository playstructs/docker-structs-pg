package payload

// OreMigrate matches structs.structs.EventOreMigrate.eventOreMigrateDetail.
// SQL handler: cache.handle_event_ore_migrate
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2012-2037)
//
// Paired transfer: ore moves from oldPrimaryAddress to primaryAddress.
// Handler writes TWO ledger rows (credit on new, debit on old) keyed by
// the same amount/block.
type OreMigrate struct {
	PrimaryAddress    string  `json:"primaryAddress"`
	OldPrimaryAddress string  `json:"oldPrimaryAddress"`
	Amount            Numeric `json:"amount"`
}
