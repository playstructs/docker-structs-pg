-- One-time backfill: add `seized_ore` to historical structs.planet_activity
-- rows of category='raid_status'.
--
-- Background: EventRaid carries seized_ore, but emitRaidStatusActivity
-- (internal/events/raid.go) historically wrote only {planet_id, fleet_id,
-- status} into detail. New writes include seized_ore; this script backfills
-- the rows written before that fix.
--
-- Source of truth: structs.ledger (mirror-raw assumed OFF, so
-- sync_state.raw_attributes is not available). structs.planet_raid is NOT
-- usable — it is keyed on planet_id and holds only the latest raid state.
--
-- Correlation for a successful raid:
--   * ledger 'seized' credit at the same block_height
--   * thief:  ledger.address      -> player_address -> player.fleet_id = detail.fleet_id
--   * victim: ledger.counterparty -> player_address.player_id = planet.owner of detail.planet_id
--
-- Non-successful raids get seized_ore="0" (no 'seized' ledger row expected).
--
-- Run manually after deploying the sync-state fix. Safe to re-run — both
-- UPDATEs guard on `NOT (detail ? 'seized_ore')`, so already-backfilled
-- rows are skipped.

\echo === PREFLIGHT: rows needing backfill ===
SELECT count(*) AS rows_missing_seized_ore
  FROM structs.planet_activity
 WHERE category = 'raid_status'
   AND NOT (detail ? 'seized_ore');

\echo === PREFLIGHT: raidSuccessful rows matchable via ledger ===
SELECT count(*) AS matchable
  FROM structs.planet_activity pa
  JOIN structs.ledger l
    ON l.block_height = pa.block_height
   AND l.action = 'seized' AND l.denom = 'ore' AND l.direction = 'credit'
  JOIN structs.player_address thief_addr ON thief_addr.address = l.address
  JOIN structs.player thief
    ON thief.id = thief_addr.player_id
   AND thief.fleet_id = pa.detail->>'fleet_id'
  JOIN structs.player_address victim_addr ON victim_addr.address = l.counterparty
  JOIN structs.planet pl
    ON pl.id = pa.planet_id
   AND pl.owner = victim_addr.player_id
 WHERE pa.category = 'raid_status'
   AND pa.detail->>'status' = 'raidSuccessful'
   AND NOT (pa.detail ? 'seized_ore');

\echo === PREFLIGHT: ambiguous matches (must be 0) ===
SELECT pa.block_height, pa.planet_id, pa.detail->>'fleet_id' AS fleet_id, count(*) AS n
  FROM structs.planet_activity pa
  JOIN structs.ledger l
    ON l.block_height = pa.block_height
   AND l.action = 'seized' AND l.denom = 'ore' AND l.direction = 'credit'
  JOIN structs.player_address thief_addr ON thief_addr.address = l.address
  JOIN structs.player thief
    ON thief.id = thief_addr.player_id
   AND thief.fleet_id = pa.detail->>'fleet_id'
  JOIN structs.player_address victim_addr ON victim_addr.address = l.counterparty
  JOIN structs.planet pl
    ON pl.id = pa.planet_id
   AND pl.owner = victim_addr.player_id
 WHERE pa.category = 'raid_status'
   AND pa.detail->>'status' = 'raidSuccessful'
   AND NOT (pa.detail ? 'seized_ore')
 GROUP BY 1, 2, 3
HAVING count(*) > 1;

BEGIN;

-- 1. Successful raids: backfill seized_ore from the matching ledger row.
UPDATE structs.planet_activity pa
   SET detail = pa.detail || jsonb_build_object('seized_ore', src.amount_p::text)
  FROM (
        SELECT pa2.planet_id,
               pa2.block_height,
               pa2.detail->>'fleet_id' AS fleet_id,
               l.amount_p
          FROM structs.planet_activity pa2
          JOIN structs.ledger l
            ON l.block_height = pa2.block_height
           AND l.action = 'seized'
           AND l.denom = 'ore'
           AND l.direction = 'credit'
          JOIN structs.player_address thief_addr ON thief_addr.address = l.address
          JOIN structs.player thief
            ON thief.id = thief_addr.player_id
           AND thief.fleet_id = pa2.detail->>'fleet_id'
          JOIN structs.player_address victim_addr ON victim_addr.address = l.counterparty
          JOIN structs.planet pl
            ON pl.id = pa2.planet_id
           AND pl.owner = victim_addr.player_id
         WHERE pa2.category = 'raid_status'
           AND pa2.detail->>'status' = 'raidSuccessful'
           AND NOT (pa2.detail ? 'seized_ore')
       ) src
 WHERE pa.category = 'raid_status'
   AND pa.planet_id = src.planet_id
   AND pa.block_height = src.block_height
   AND pa.detail->>'fleet_id' = src.fleet_id
   AND NOT (pa.detail ? 'seized_ore');

-- 2. Everything else (failed/in-progress raids, plus any successful raid with
--    no ledger match): seized_ore = "0".
UPDATE structs.planet_activity
   SET detail = detail || jsonb_build_object('seized_ore', '0')
 WHERE category = 'raid_status'
   AND NOT (detail ? 'seized_ore')
   AND detail->>'status' IS DISTINCT FROM 'raidSuccessful';

-- 3. Postflight: any raidSuccessful row still missing seized_ore is unmatched
--    (no ledger 'seized' credit could be correlated — e.g. a success that
--    stole 0 ore, or a data gap). These are INTENTIONALLY left untouched
--    (not zeroed) so the operator can investigate and decide. Re-running
--    this script will not change them; set seized_ore manually if "0" is
--    confirmed correct for a given row.
\echo === POSTFLIGHT: unmatched raidSuccessful rows (investigate) ===
SELECT block_height, planet_id, detail->>'fleet_id' AS fleet_id
  FROM structs.planet_activity
 WHERE category = 'raid_status'
   AND detail->>'status' = 'raidSuccessful'
   AND NOT (detail ? 'seized_ore')
 ORDER BY block_height;

COMMIT;

\echo === DONE: remaining raid_status rows without seized_ore ===
SELECT count(*) AS still_missing
  FROM structs.planet_activity
 WHERE category = 'raid_status'
   AND NOT (detail ? 'seized_ore');
