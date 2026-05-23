package payload

// OreMine matches structs.structs.EventOreMine.eventOreMineDetail.
// SQL handler: cache.handle_event_ore_mine
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1990-2010)
//
// amount is NUMERIC ore (no µ conversion). Bind via Numeric.PgValue() so
// PG NUMERIC precision survives the round-trip (Go float64 would lose it).
type OreMine struct {
	PrimaryAddress string  `json:"primaryAddress"`
	Amount         Numeric `json:"amount"`
}
