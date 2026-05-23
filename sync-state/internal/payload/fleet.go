package payload

// Fleet matches structs.structs.EventFleet.fleet.
// SQL handler: cache.handle_event_fleet
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:321-430)
//
// space/air/land/water are nested jsonb objects from the chain — keep them
// raw so the handler can re-emit `jsonb_build_object('space', ...) || ...`
// without round-tripping through Go types.
type Fleet struct {
	ID                    string  `json:"id"`
	Owner                 string  `json:"owner"`
	Space                 Raw     `json:"space"`
	Air                   Raw     `json:"air"`
	Land                  Raw     `json:"land"`
	Water                 Raw     `json:"water"`
	SpaceSlots            JSONInt `json:"spaceSlots"`
	AirSlots              JSONInt `json:"airSlots"`
	LandSlots             JSONInt `json:"landSlots"`
	WaterSlots            JSONInt `json:"waterSlots"`
	LocationType          string  `json:"locationType"`
	LocationID            string  `json:"locationId"`
	Status                string  `json:"status"`
	LocationListForward   string  `json:"locationListForward"`
	LocationListBackward  string  `json:"locationListBackward"`
	CommandStruct         string  `json:"commandStruct"`
}
