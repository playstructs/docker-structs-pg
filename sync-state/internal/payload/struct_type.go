package payload

// StructType matches structs.structs.EventStructType.structType.
// SQL handler: cache.handle_event_struct_type
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:885-1389)
//
// 65 chain fields. buildDraw/passiveDraw map to *_p columns (precision).
// is_command is derived in Go from class == "Command Ship".
//
// Several of the array-shaped columns (possible_ambit_array,
// primary_weapon_ambits_array, secondary_weapon_ambits_array) are GENERATED
// from their bitmask integer source columns and must not be written.
type StructType struct {
	ID                                       JSONInt `json:"id"`
	Type                                     string  `json:"type"`
	Category                                 string  `json:"category"`
	BuildLimit                               JSONInt `json:"buildLimit"`
	BuildDifficulty                          JSONInt `json:"buildDifficulty"`
	BuildDraw                                JSONInt `json:"buildDraw"`
	MaxHealth                                JSONInt `json:"maxHealth"`
	PassiveDraw                              JSONInt `json:"passiveDraw"`
	PossibleAmbit                            JSONInt `json:"possibleAmbit"`
	Movable                                  JSONBool `json:"movable"`
	SlotBound                                JSONBool `json:"slotBound"`

	PrimaryWeapon                            string   `json:"primaryWeapon"`
	PrimaryWeaponControl                     string   `json:"primaryWeaponControl"`
	PrimaryWeaponCharge                      JSONInt  `json:"primaryWeaponCharge"`
	PrimaryWeaponAmbits                      JSONInt  `json:"primaryWeaponAmbits"`
	PrimaryWeaponTargets                     JSONInt  `json:"primaryWeaponTargets"`
	PrimaryWeaponShots                       JSONInt  `json:"primaryWeaponShots"`
	PrimaryWeaponDamage                      JSONInt  `json:"primaryWeaponDamage"`
	PrimaryWeaponBlockable                   JSONBool `json:"primaryWeaponBlockable"`
	PrimaryWeaponCounterable                 JSONBool `json:"primaryWeaponCounterable"`
	PrimaryWeaponRecoilDamage                JSONInt  `json:"primaryWeaponRecoilDamage"`
	PrimaryWeaponShotSuccessRateNumerator    JSONInt  `json:"primaryWeaponShotSuccessRateNumerator"`
	PrimaryWeaponShotSuccessRateDenominator  JSONInt  `json:"primaryWeaponShotSuccessRateDenominator"`

	SecondaryWeapon                            string   `json:"secondaryWeapon"`
	SecondaryWeaponControl                     string   `json:"secondaryWeaponControl"`
	SecondaryWeaponCharge                      JSONInt  `json:"secondaryWeaponCharge"`
	SecondaryWeaponAmbits                      JSONInt  `json:"secondaryWeaponAmbits"`
	SecondaryWeaponTargets                     JSONInt  `json:"secondaryWeaponTargets"`
	SecondaryWeaponShots                       JSONInt  `json:"secondaryWeaponShots"`
	SecondaryWeaponDamage                      JSONInt  `json:"secondaryWeaponDamage"`
	SecondaryWeaponBlockable                   JSONBool `json:"secondaryWeaponBlockable"`
	SecondaryWeaponCounterable                 JSONBool `json:"secondaryWeaponCounterable"`
	SecondaryWeaponRecoilDamage                JSONInt  `json:"secondaryWeaponRecoilDamage"`
	SecondaryWeaponShotSuccessRateNumerator    JSONInt  `json:"secondaryWeaponShotSuccessRateNumerator"`
	SecondaryWeaponShotSuccessRateDenominator  JSONInt  `json:"secondaryWeaponShotSuccessRateDenominator"`

	PassiveWeaponry     string `json:"passiveWeaponry"`
	UnitDefenses        string `json:"unitDefenses"`
	OreReserveDefenses  string `json:"oreReserveDefenses"`
	PlanetaryDefenses   string `json:"planetaryDefenses"`
	PlanetaryMining     string `json:"planetaryMining"`
	PlanetaryRefinery   string `json:"planetaryRefinery"`
	PowerGeneration     string `json:"powerGeneration"`

	ActivateCharge        JSONInt `json:"activateCharge"`
	BuildCharge           JSONInt `json:"buildCharge"`
	DefendChangeCharge    JSONInt `json:"defendChangeCharge"`
	MoveCharge            JSONInt `json:"moveCharge"`
	StealthActivateCharge JSONInt `json:"stealthActivateCharge"`

	AttackReduction              JSONInt  `json:"attackReduction"`
	AttackCounterable            JSONBool `json:"attackCounterable"`
	StealthSystems               JSONBool `json:"stealthSystems"`
	CounterAttack                JSONInt  `json:"counterAttack"`
	CounterAttackSameAmbit       JSONInt  `json:"counterAttackSameAmbit"`
	PostDestructionDamage        JSONInt  `json:"postDestructionDamage"`
	GeneratingRate               JSONInt  `json:"generatingRate"`
	PlanetaryShieldContribution  JSONInt  `json:"planetaryShieldContribution"`
	OreMiningDifficulty          JSONInt  `json:"oreMiningDifficulty"`
	OreRefiningDifficulty        JSONInt  `json:"oreRefiningDifficulty"`

	UnguidedDefensiveSuccessRateNumerator   JSONInt `json:"unguidedDefensiveSuccessRateNumerator"`
	UnguidedDefensiveSuccessRateDenominator JSONInt `json:"unguidedDefensiveSuccessRateDenominator"`
	GuidedDefensiveSuccessRateNumerator     JSONInt `json:"guidedDefensiveSuccessRateNumerator"`
	GuidedDefensiveSuccessRateDenominator   JSONInt `json:"guidedDefensiveSuccessRateDenominator"`

	TriggerRaidDefeatByDestruction JSONBool `json:"triggerRaidDefeatByDestruction"`

	Class                       string `json:"class"`
	ClassAbbreviation           string `json:"classAbbreviation"`
	DefaultCosmeticModelNumber  string `json:"defaultCosmeticModelNumber"`
	DefaultCosmeticName         string `json:"defaultCosmeticName"`
}

// IsCommand mirrors the SQL handler's `(v.class = 'Command Ship')` derived
// column. Computed in Go so we don't depend on a chain-emitted is_command
// (which doesn't exist).
func (s StructType) IsCommand() bool {
	return s.Class == "Command Ship"
}
