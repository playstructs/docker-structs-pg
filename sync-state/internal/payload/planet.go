package payload

// Planet matches structs.structs.EventPlanet.planet.
// SQL handler: cache.handle_event_planet
// (cache-trigger-add-queue-20260427-ugc-fields.sql:312-419)
//
// space/air/land/water mirror Fleet — raw jsonb, handler reassembles map.
// Name is chain UGC; handler only writes planet_meta.name when non-empty
// (preserves NAME_PLANET trigger's generate_planet_name() default).
type Planet struct {
	ID                string  `json:"id"`
	MaxOre            JSONInt `json:"maxOre"`
	Creator           string  `json:"creator"`
	Owner             string  `json:"owner"`
	Space             Raw     `json:"space"`
	Air               Raw     `json:"air"`
	Land              Raw     `json:"land"`
	Water             Raw     `json:"water"`
	SpaceSlots        JSONInt `json:"spaceSlots"`
	AirSlots          JSONInt `json:"airSlots"`
	LandSlots         JSONInt `json:"landSlots"`
	WaterSlots        JSONInt `json:"waterSlots"`
	Status            string  `json:"status"`
	LocationListStart string  `json:"locationListStart"`
	LocationListEnd   string  `json:"locationListEnd"`
	Name              string  `json:"name"`
}
