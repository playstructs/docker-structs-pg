package payload

// Infusion matches structs.structs.EventInfusion.infusion.
// SQL handler: cache.handle_event_infusion
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:248-319)
//
// Note: fuel/defusing/power/ratio are written to *_p columns (precision-
// preserving base). The chain emits raw integer-precision values that map
// 1:1 to those columns; the *non*-_p columns are GENERATED in the schema
// and must NOT be written by the handler.
type Infusion struct {
	DestinationID   string  `json:"destinationId"`
	Address         string  `json:"address"`
	DestinationType string  `json:"destinationType"`
	PlayerID        string  `json:"playerId"`
	Fuel            Numeric `json:"fuel"`
	Defusing        Numeric `json:"defusing"`
	Power           Numeric `json:"power"`
	Ratio           Numeric `json:"ratio"`
	Commission      Numeric `json:"commission"`
}
