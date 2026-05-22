// Package derivations was originally intended to hold the Go ports of
// state-change-driven derivations that ran as PG triggers on structs.*
// tables:
//
//   - planet_activity_struct_movement (struct location change)
//   - planet_activity_fleet_move      (fleet INSERT/UPDATE)
//   - planet_activity_raid_status     (raid INSERT/UPDATE)
//   - planet_activity_struct_attribute (struct_attribute INSERT/UPDATE/DELETE)
//   - update_address_guild            (player.guild_id cascade)
//   - infusion_ledger                 (infusion fuel delta → ledger pairs)
//
// In practice every derivation lives next to the event handler that
// triggers it (internal/events/{struct,fleet,raid,struct_attribute,
// player,infusion}.go) — keeping them co-located means the SQL trigger
// porting and the event handler share one file each, easier to reason
// about than a parallel package.
//
// This package is retained as a placeholder so external references keep
// resolving; new code should not be added here.
package derivations
