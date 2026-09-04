-- One-time re-seed of structs.planet_attribute from the chain LCD store.
--
-- Symptom: view.work MINE/REFINE block_start lags the chain for planets
-- whose ore clocks (attr 12/13) last changed before a sync-state cutover.
-- EventPlanetAttribute only rewrites when val changes, so missed events
-- never heal until the next chain write — which a stale clock itself
-- prevents for clients solving from view.work.
--
-- Preferred fix going forward: restart sync-state with
-- SYNC_STATE_PLANET_ATTRIBUTE_SWEEP=true (default). This script is for
-- operators who need the repair without waiting on a deploy/restart.
--
-- Prerequisites:
--   1. Export the LCD store (one page is enough at limit 100000):
--
--        LCD=https://<lcd host>
--        curl -s "$LCD/structs/structs/planet_attribute?pagination.limit=100000" \
--          | jq -r '.planetAttributeRecords[] | [.attributeId, .value] | @tsv' \
--          > /tmp/planet_attribute.tsv
--
--   2. Run this script from psql with \copy able to read that TSV.
--      Prefer stopping sync-state first; concurrent handler upserts for
--      the same id simply write the chain's current value either way.
--
-- Does NOT emit planet_activity / planet_activity_notify. Clients re-read
-- the store. After this (and after the writer is current), add
-- attribute_type NOT NULL on production (see structs-app handoff).

\echo === PREFLIGHT: 12/13 rows untouched since ore-clock cutover (example) ===
SELECT count(*) FILTER (WHERE updated_at <  '2026-09-04 19:53+00') AS before_cutover,
       count(*) FILTER (WHERE updated_at >= '2026-09-04 19:53+00') AS after_cutover
  FROM structs.planet_attribute
 WHERE split_part(id, '-', 1) IN ('12', '13');

BEGIN;

CREATE TEMP TABLE chain_pa (id text PRIMARY KEY, val bigint);
\copy chain_pa FROM '/tmp/planet_attribute.tsv'

-- attributeId grammar: "<attrType>-<objectTypeId>-<objectIndex>"; planet rows
-- are 3 segments with objectTypeId 2.
INSERT INTO structs.planet_attribute (id, object_id, object_type, attribute_type, val, updated_at)
SELECT c.id,
       split_part(c.id, '-', 2) || '-' || split_part(c.id, '-', 3)  AS object_id,
       'planet'                                                       AS object_type,
       CASE split_part(c.id, '-', 1)
         WHEN '0'  THEN 'planetaryShield'
         WHEN '1'  THEN 'repairNetworkQuantity'
         WHEN '2'  THEN 'defensiveCannonQuantity'
         WHEN '3'  THEN 'coordinatedGlobalShieldNetworkQuantity'
         WHEN '4'  THEN 'lowOrbitBallisticsInterceptorNetworkQuantity'
         WHEN '5'  THEN 'advancedLowOrbitBallisticsInterceptorNetworkQuantity'
         WHEN '6'  THEN 'lowOrbitBallisticsInterceptorNetworkSuccessRateNumerator'
         WHEN '7'  THEN 'lowOrbitBallisticsInterceptorNetworkSuccessRateDenominator'
         WHEN '8'  THEN 'orbitalJammingStationQuantity'
         WHEN '9'  THEN 'advancedOrbitalJammingStationQuantity'
         WHEN '10' THEN 'blockStartRaid'
         WHEN '11' THEN 'blockRaiderArrived'
         WHEN '12' THEN 'blockStartOreMine'
         WHEN '13' THEN 'blockStartOreRefine'
         WHEN '14' THEN 'oreMiningActiveQuantity'
         WHEN '15' THEN 'oreRefiningActiveQuantity'
       END                                                            AS attribute_type,
       c.val,
       now()
  FROM chain_pa c
 WHERE array_length(string_to_array(c.id, '-'), 1) = 3
   AND split_part(c.id, '-', 2) = '2'
   AND split_part(c.id, '-', 1) IN (
         '0','1','2','3','4','5','6','7','8','9','10','11','12','13','14','15'
       )
ON CONFLICT (id) DO UPDATE
   SET val            = EXCLUDED.val,
       attribute_type = EXCLUDED.attribute_type,
       object_id      = EXCLUDED.object_id,
       object_type    = EXCLUDED.object_type,
       updated_at     = EXCLUDED.updated_at
 WHERE structs.planet_attribute.val            IS DISTINCT FROM EXCLUDED.val
    OR structs.planet_attribute.attribute_type IS DISTINCT FROM EXCLUDED.attribute_type
    OR structs.planet_attribute.object_id      IS DISTINCT FROM EXCLUDED.object_id
    OR structs.planet_attribute.object_type    IS DISTINCT FROM EXCLUDED.object_type;

-- Rows the chain no longer holds (cleared to zero and pruned) → val 0,
-- matching the handler keep-zero rule.
UPDATE structs.planet_attribute p
   SET val = 0, updated_at = now()
 WHERE split_part(p.id, '-', 2) = '2'
   AND p.val IS DISTINCT FROM 0
   AND NOT EXISTS (SELECT 1 FROM chain_pa c WHERE c.id = p.id);

COMMIT;

\echo === POSTFLIGHT: spot-check planet 2-27693 (expect chain blockStartOreMine) ===
SELECT id, attribute_type, val, updated_at
  FROM structs.planet_attribute
 WHERE id IN ('12-2-27693', '13-2-27693');
