package payload

// Raid matches structs.structs.EventRaid.eventRaidDetail.
// SQL handler: cache.handle_event_raid — final form in
// cache-trigger-add-queue-20260226-add-seized-ore-better.sql:5-43.
//
// `seized_ore` is snake_case in the chain payload (the 20260226 migration
// fixed the camelCase typo in 20260221). It's NUMERIC, nullable, only
// populated on raid-completion events.
//
// The handler upserts structs.planet_raid with an IS DISTINCT FROM guard
// on (fleet_id, status) only — seized_ore-only updates are intentionally
// not gated (matches the SQL's WHERE clause; the trigger on planet_raid
// emits the planet_activity row regardless).
type Raid struct {
	FleetID   string  `json:"fleetId"`
	PlanetID  string  `json:"planetId"`
	Status    string  `json:"status"`
	SeizedOre Numeric `json:"seized_ore"`
}
