package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// structTypeHandler ports cache.handle_event_struct_type
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:885-1389).
//
// 65 writable columns. build_draw/passive_draw map to *_p columns; their
// non-_p companions are GENERATED. is_command is derived in Go from
// class == 'Command Ship'. The *_array bitmask-derivative columns are
// also GENERATED and never written here.
//
// The IS DISTINCT FROM guard compares all 65 writable columns as a single
// row tuple (matches the SQL handler's row-comparison form). We keep the
// same tuple shape to ensure spurious updates are suppressed identically.
type structTypeHandler struct{}

func (structTypeHandler) CompositeKey() string {
	return "structs.structs.EventStructType.structType"
}

const structTypeUpsertSQL = `
INSERT INTO structs.struct_type (
    id, type, category,
    build_limit, build_difficulty, build_draw_p, max_health, passive_draw_p, possible_ambit,
    movable, slot_bound,
    primary_weapon, primary_weapon_control, primary_weapon_charge,
    primary_weapon_ambits, primary_weapon_targets, primary_weapon_shots, primary_weapon_damage,
    primary_weapon_blockable, primary_weapon_counterable, primary_weapon_recoil_damage,
    primary_weapon_shot_success_rate_numerator, primary_weapon_shot_success_rate_denominator,
    secondary_weapon, secondary_weapon_control, secondary_weapon_charge,
    secondary_weapon_ambits, secondary_weapon_targets, secondary_weapon_shots, secondary_weapon_damage,
    secondary_weapon_blockable, secondary_weapon_counterable, secondary_weapon_recoil_damage,
    secondary_weapon_shot_success_rate_numerator, secondary_weapon_shot_success_rate_denominator,
    passive_weaponry, unit_defenses, ore_reserve_defenses, planetary_defenses,
    planetary_mining, planetary_refinery, power_generation,
    activate_charge, build_charge, defend_change_charge, move_charge, stealth_activate_charge,
    attack_reduction, attack_counterable, stealth_systems,
    counter_attack, counter_attack_same_ambit, post_destruction_damage,
    generating_rate, planetary_shield_contribution,
    ore_mining_difficulty, ore_refining_difficulty,
    unguided_defensive_success_rate_numerator, unguided_defensive_success_rate_denominator,
    guided_defensive_success_rate_numerator, guided_defensive_success_rate_denominator,
    trigger_raid_defeat_by_destruction,
    updated_at,
    class, class_abbreviation, default_cosmetic_model_number, default_cosmetic_name,
    is_command
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7, $8, $9,
    $10, $11,
    $12, $13, $14,
    $15, $16, $17, $18,
    $19, $20, $21,
    $22, $23,
    $24, $25, $26,
    $27, $28, $29, $30,
    $31, $32, $33,
    $34, $35,
    $36, $37, $38, $39,
    $40, $41, $42,
    $43, $44, $45, $46, $47,
    $48, $49, $50,
    $51, $52, $53,
    $54, $55,
    $56, $57,
    $58, $59,
    $60, $61,
    $62,
    NOW(),
    $63, $64, $65, $66,
    $67
)
ON CONFLICT (id) DO UPDATE
   SET type = EXCLUDED.type,
       category = EXCLUDED.category,
       build_limit = EXCLUDED.build_limit,
       build_difficulty = EXCLUDED.build_difficulty,
       build_draw_p = EXCLUDED.build_draw_p,
       max_health = EXCLUDED.max_health,
       passive_draw_p = EXCLUDED.passive_draw_p,
       possible_ambit = EXCLUDED.possible_ambit,
       movable = EXCLUDED.movable,
       slot_bound = EXCLUDED.slot_bound,
       primary_weapon = EXCLUDED.primary_weapon,
       primary_weapon_control = EXCLUDED.primary_weapon_control,
       primary_weapon_charge = EXCLUDED.primary_weapon_charge,
       primary_weapon_ambits = EXCLUDED.primary_weapon_ambits,
       primary_weapon_targets = EXCLUDED.primary_weapon_targets,
       primary_weapon_shots = EXCLUDED.primary_weapon_shots,
       primary_weapon_damage = EXCLUDED.primary_weapon_damage,
       primary_weapon_blockable = EXCLUDED.primary_weapon_blockable,
       primary_weapon_counterable = EXCLUDED.primary_weapon_counterable,
       primary_weapon_recoil_damage = EXCLUDED.primary_weapon_recoil_damage,
       primary_weapon_shot_success_rate_numerator = EXCLUDED.primary_weapon_shot_success_rate_numerator,
       primary_weapon_shot_success_rate_denominator = EXCLUDED.primary_weapon_shot_success_rate_denominator,
       secondary_weapon = EXCLUDED.secondary_weapon,
       secondary_weapon_control = EXCLUDED.secondary_weapon_control,
       secondary_weapon_charge = EXCLUDED.secondary_weapon_charge,
       secondary_weapon_ambits = EXCLUDED.secondary_weapon_ambits,
       secondary_weapon_targets = EXCLUDED.secondary_weapon_targets,
       secondary_weapon_shots = EXCLUDED.secondary_weapon_shots,
       secondary_weapon_damage = EXCLUDED.secondary_weapon_damage,
       secondary_weapon_blockable = EXCLUDED.secondary_weapon_blockable,
       secondary_weapon_counterable = EXCLUDED.secondary_weapon_counterable,
       secondary_weapon_recoil_damage = EXCLUDED.secondary_weapon_recoil_damage,
       secondary_weapon_shot_success_rate_numerator = EXCLUDED.secondary_weapon_shot_success_rate_numerator,
       secondary_weapon_shot_success_rate_denominator = EXCLUDED.secondary_weapon_shot_success_rate_denominator,
       passive_weaponry = EXCLUDED.passive_weaponry,
       unit_defenses = EXCLUDED.unit_defenses,
       ore_reserve_defenses = EXCLUDED.ore_reserve_defenses,
       planetary_defenses = EXCLUDED.planetary_defenses,
       planetary_mining = EXCLUDED.planetary_mining,
       planetary_refinery = EXCLUDED.planetary_refinery,
       power_generation = EXCLUDED.power_generation,
       activate_charge = EXCLUDED.activate_charge,
       build_charge = EXCLUDED.build_charge,
       defend_change_charge = EXCLUDED.defend_change_charge,
       move_charge = EXCLUDED.move_charge,
       stealth_activate_charge = EXCLUDED.stealth_activate_charge,
       attack_reduction = EXCLUDED.attack_reduction,
       attack_counterable = EXCLUDED.attack_counterable,
       stealth_systems = EXCLUDED.stealth_systems,
       counter_attack = EXCLUDED.counter_attack,
       counter_attack_same_ambit = EXCLUDED.counter_attack_same_ambit,
       post_destruction_damage = EXCLUDED.post_destruction_damage,
       generating_rate = EXCLUDED.generating_rate,
       planetary_shield_contribution = EXCLUDED.planetary_shield_contribution,
       ore_mining_difficulty = EXCLUDED.ore_mining_difficulty,
       ore_refining_difficulty = EXCLUDED.ore_refining_difficulty,
       unguided_defensive_success_rate_numerator = EXCLUDED.unguided_defensive_success_rate_numerator,
       unguided_defensive_success_rate_denominator = EXCLUDED.unguided_defensive_success_rate_denominator,
       guided_defensive_success_rate_numerator = EXCLUDED.guided_defensive_success_rate_numerator,
       guided_defensive_success_rate_denominator = EXCLUDED.guided_defensive_success_rate_denominator,
       trigger_raid_defeat_by_destruction = EXCLUDED.trigger_raid_defeat_by_destruction,
       updated_at = NOW(),
       class = EXCLUDED.class,
       class_abbreviation = EXCLUDED.class_abbreviation,
       default_cosmetic_model_number = EXCLUDED.default_cosmetic_model_number,
       default_cosmetic_name = EXCLUDED.default_cosmetic_name,
       is_command = EXCLUDED.is_command
 WHERE (
    structs.struct_type.type,
    structs.struct_type.category,
    structs.struct_type.build_limit,
    structs.struct_type.build_difficulty,
    structs.struct_type.build_draw_p,
    structs.struct_type.max_health,
    structs.struct_type.passive_draw_p,
    structs.struct_type.possible_ambit,
    structs.struct_type.movable,
    structs.struct_type.slot_bound,
    structs.struct_type.primary_weapon,
    structs.struct_type.primary_weapon_control,
    structs.struct_type.primary_weapon_charge,
    structs.struct_type.primary_weapon_ambits,
    structs.struct_type.primary_weapon_targets,
    structs.struct_type.primary_weapon_shots,
    structs.struct_type.primary_weapon_damage,
    structs.struct_type.primary_weapon_blockable,
    structs.struct_type.primary_weapon_counterable,
    structs.struct_type.primary_weapon_recoil_damage,
    structs.struct_type.primary_weapon_shot_success_rate_numerator,
    structs.struct_type.primary_weapon_shot_success_rate_denominator,
    structs.struct_type.secondary_weapon,
    structs.struct_type.secondary_weapon_control,
    structs.struct_type.secondary_weapon_charge,
    structs.struct_type.secondary_weapon_ambits,
    structs.struct_type.secondary_weapon_targets,
    structs.struct_type.secondary_weapon_shots,
    structs.struct_type.secondary_weapon_damage,
    structs.struct_type.secondary_weapon_blockable,
    structs.struct_type.secondary_weapon_counterable,
    structs.struct_type.secondary_weapon_recoil_damage,
    structs.struct_type.secondary_weapon_shot_success_rate_numerator,
    structs.struct_type.secondary_weapon_shot_success_rate_denominator,
    structs.struct_type.passive_weaponry,
    structs.struct_type.unit_defenses,
    structs.struct_type.ore_reserve_defenses,
    structs.struct_type.planetary_defenses,
    structs.struct_type.planetary_mining,
    structs.struct_type.planetary_refinery,
    structs.struct_type.power_generation,
    structs.struct_type.activate_charge,
    structs.struct_type.build_charge,
    structs.struct_type.defend_change_charge,
    structs.struct_type.move_charge,
    structs.struct_type.stealth_activate_charge,
    structs.struct_type.attack_reduction,
    structs.struct_type.attack_counterable,
    structs.struct_type.stealth_systems,
    structs.struct_type.counter_attack,
    structs.struct_type.counter_attack_same_ambit,
    structs.struct_type.post_destruction_damage,
    structs.struct_type.generating_rate,
    structs.struct_type.planetary_shield_contribution,
    structs.struct_type.ore_mining_difficulty,
    structs.struct_type.ore_refining_difficulty,
    structs.struct_type.unguided_defensive_success_rate_numerator,
    structs.struct_type.unguided_defensive_success_rate_denominator,
    structs.struct_type.guided_defensive_success_rate_numerator,
    structs.struct_type.guided_defensive_success_rate_denominator,
    structs.struct_type.trigger_raid_defeat_by_destruction,
    structs.struct_type.class,
    structs.struct_type.class_abbreviation,
    structs.struct_type.default_cosmetic_model_number,
    structs.struct_type.default_cosmetic_name,
    structs.struct_type.is_command
 ) IS DISTINCT FROM (
    EXCLUDED.type,
    EXCLUDED.category,
    EXCLUDED.build_limit,
    EXCLUDED.build_difficulty,
    EXCLUDED.build_draw_p,
    EXCLUDED.max_health,
    EXCLUDED.passive_draw_p,
    EXCLUDED.possible_ambit,
    EXCLUDED.movable,
    EXCLUDED.slot_bound,
    EXCLUDED.primary_weapon,
    EXCLUDED.primary_weapon_control,
    EXCLUDED.primary_weapon_charge,
    EXCLUDED.primary_weapon_ambits,
    EXCLUDED.primary_weapon_targets,
    EXCLUDED.primary_weapon_shots,
    EXCLUDED.primary_weapon_damage,
    EXCLUDED.primary_weapon_blockable,
    EXCLUDED.primary_weapon_counterable,
    EXCLUDED.primary_weapon_recoil_damage,
    EXCLUDED.primary_weapon_shot_success_rate_numerator,
    EXCLUDED.primary_weapon_shot_success_rate_denominator,
    EXCLUDED.secondary_weapon,
    EXCLUDED.secondary_weapon_control,
    EXCLUDED.secondary_weapon_charge,
    EXCLUDED.secondary_weapon_ambits,
    EXCLUDED.secondary_weapon_targets,
    EXCLUDED.secondary_weapon_shots,
    EXCLUDED.secondary_weapon_damage,
    EXCLUDED.secondary_weapon_blockable,
    EXCLUDED.secondary_weapon_counterable,
    EXCLUDED.secondary_weapon_recoil_damage,
    EXCLUDED.secondary_weapon_shot_success_rate_numerator,
    EXCLUDED.secondary_weapon_shot_success_rate_denominator,
    EXCLUDED.passive_weaponry,
    EXCLUDED.unit_defenses,
    EXCLUDED.ore_reserve_defenses,
    EXCLUDED.planetary_defenses,
    EXCLUDED.planetary_mining,
    EXCLUDED.planetary_refinery,
    EXCLUDED.power_generation,
    EXCLUDED.activate_charge,
    EXCLUDED.build_charge,
    EXCLUDED.defend_change_charge,
    EXCLUDED.move_charge,
    EXCLUDED.stealth_activate_charge,
    EXCLUDED.attack_reduction,
    EXCLUDED.attack_counterable,
    EXCLUDED.stealth_systems,
    EXCLUDED.counter_attack,
    EXCLUDED.counter_attack_same_ambit,
    EXCLUDED.post_destruction_damage,
    EXCLUDED.generating_rate,
    EXCLUDED.planetary_shield_contribution,
    EXCLUDED.ore_mining_difficulty,
    EXCLUDED.ore_refining_difficulty,
    EXCLUDED.unguided_defensive_success_rate_numerator,
    EXCLUDED.unguided_defensive_success_rate_denominator,
    EXCLUDED.guided_defensive_success_rate_numerator,
    EXCLUDED.guided_defensive_success_rate_denominator,
    EXCLUDED.trigger_raid_defeat_by_destruction,
    EXCLUDED.class,
    EXCLUDED.class_abbreviation,
    EXCLUDED.default_cosmetic_model_number,
    EXCLUDED.default_cosmetic_name,
    EXCLUDED.is_command
 )`

func (structTypeHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.StructType](raw)
	if err != nil {
		return err
	}
	if p.ID == 0 {
		return fmt.Errorf("struct_type: zero id")
	}

	// Positional arg list: must align with $1..$67 in structTypeUpsertSQL.
	// $62 is trigger_raid_defeat_by_destruction; $63-66 are class/cosmetic
	// fields; $67 is the derived is_command.
	args := []any{
		// $1..$3
		p.ID.Int64(), payload.NullableText(p.Type), payload.NullableText(p.Category),
		// $4..$9: build/health/draw/possible_ambit
		p.BuildLimit.Int64(), p.BuildDifficulty.Int64(),
		p.BuildDraw.Int64(),
		p.MaxHealth.Int64(),
		p.PassiveDraw.Int64(),
		p.PossibleAmbit.Int64(),
		// $10, $11 booleans
		p.Movable.Bool(), p.SlotBound.Bool(),
		// $12..$14 primary weapon trio
		payload.NullableText(p.PrimaryWeapon), payload.NullableText(p.PrimaryWeaponControl),
		p.PrimaryWeaponCharge.Int64(),
		// $15..$18
		p.PrimaryWeaponAmbits.Int64(), p.PrimaryWeaponTargets.Int64(),
		p.PrimaryWeaponShots.Int64(), p.PrimaryWeaponDamage.Int64(),
		// $19..$21
		p.PrimaryWeaponBlockable.Bool(), p.PrimaryWeaponCounterable.Bool(),
		p.PrimaryWeaponRecoilDamage.Int64(),
		// $22..$23
		p.PrimaryWeaponShotSuccessRateNumerator.Int64(),
		p.PrimaryWeaponShotSuccessRateDenominator.Int64(),
		// $24..$26 secondary weapon trio
		payload.NullableText(p.SecondaryWeapon), payload.NullableText(p.SecondaryWeaponControl),
		p.SecondaryWeaponCharge.Int64(),
		// $27..$30
		p.SecondaryWeaponAmbits.Int64(), p.SecondaryWeaponTargets.Int64(),
		p.SecondaryWeaponShots.Int64(), p.SecondaryWeaponDamage.Int64(),
		// $31..$33
		p.SecondaryWeaponBlockable.Bool(), p.SecondaryWeaponCounterable.Bool(),
		p.SecondaryWeaponRecoilDamage.Int64(),
		// $34..$35
		p.SecondaryWeaponShotSuccessRateNumerator.Int64(),
		p.SecondaryWeaponShotSuccessRateDenominator.Int64(),
		// $36..$39 passive_weaponry / unit_defenses / ore_reserve_defenses / planetary_defenses
		payload.NullableText(p.PassiveWeaponry), payload.NullableText(p.UnitDefenses),
		payload.NullableText(p.OreReserveDefenses), payload.NullableText(p.PlanetaryDefenses),
		// $40..$42
		payload.NullableText(p.PlanetaryMining), payload.NullableText(p.PlanetaryRefinery),
		payload.NullableText(p.PowerGeneration),
		// $43..$47 charges
		p.ActivateCharge.Int64(), p.BuildCharge.Int64(), p.DefendChangeCharge.Int64(),
		p.MoveCharge.Int64(), p.StealthActivateCharge.Int64(),
		// $48..$50
		p.AttackReduction.Int64(), p.AttackCounterable.Bool(), p.StealthSystems.Bool(),
		// $51..$53
		p.CounterAttack.Int64(), p.CounterAttackSameAmbit.Int64(), p.PostDestructionDamage.Int64(),
		// $54..$55
		p.GeneratingRate.Int64(), p.PlanetaryShieldContribution.Int64(),
		// $56..$57
		p.OreMiningDifficulty.Int64(), p.OreRefiningDifficulty.Int64(),
		// $58..$59 unguided defensive rate
		p.UnguidedDefensiveSuccessRateNumerator.Int64(),
		p.UnguidedDefensiveSuccessRateDenominator.Int64(),
		// $60..$61 guided defensive rate
		p.GuidedDefensiveSuccessRateNumerator.Int64(),
		p.GuidedDefensiveSuccessRateDenominator.Int64(),
		// $62
		p.TriggerRaidDefeatByDestruction.Bool(),
		// $63..$66 class + cosmetic
		payload.NullableText(p.Class),
		payload.NullableText(p.ClassAbbreviation),
		payload.NullableText(p.DefaultCosmeticModelNumber),
		payload.NullableText(p.DefaultCosmeticName),
		// $67 derived is_command
		p.IsCommand(),
	}

	if _, err := tx.Exec(ctx, structTypeUpsertSQL, args...); err != nil {
		return fmt.Errorf("struct_type upsert id=%d: %w", p.ID.Int64(), err)
	}
	return nil
}
