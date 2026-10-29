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
	DestinationID     string  `json:"destinationId"`
	Address           string  `json:"address"`
	DestinationType   string  `json:"destinationType"`
	PlayerID          string  `json:"playerId"`
	Fuel              Uint64  `json:"fuel"`
	Defusing          Uint64  `json:"defusing"`
	Power             Uint64  `json:"power"`
	Ratio             Uint64  `json:"ratio"`
	Commission        Numeric `json:"commission"`
	OwnerID           string  `json:"ownerId"`
	GeneratorDisabled bool    `json:"generatorDisabled"`
}

func (p Infusion) AssetOwnerID() string {
	if p.OwnerID != "" {
		return p.OwnerID
	}
	return p.PlayerID
}
