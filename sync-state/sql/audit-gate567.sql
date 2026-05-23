\timing on
\pset format aligned
\pset border 2
\set chain 'structstestnet-111'

\echo
\echo === GATE 5: ledger zero-sum invariant (per-denom credits == debits) ===
\echo --- for native chain denoms total credit should equal total debit ---
\echo --- (excludes 'genesis' action since that's an out-of-band credit only) ---
SELECT denom,
       SUM(CASE direction WHEN 'credit' THEN amount ELSE 0 END) AS total_credit,
       SUM(CASE direction WHEN 'debit'  THEN amount ELSE 0 END) AS total_debit,
       SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END) AS net,
       COUNT(*) AS n_entries
  FROM structs.ledger
 WHERE action <> 'genesis'
 GROUP BY denom
 ORDER BY n_entries DESC;

\echo
\echo --- including genesis (positive net per denom is the genesis allocation) ---
SELECT denom,
       SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END) AS net_all_actions,
       SUM(CASE WHEN action='genesis' AND direction='credit' THEN amount ELSE 0 END) AS from_genesis,
       SUM(CASE WHEN action='genesis' THEN 0
                WHEN direction='credit' THEN amount ELSE -amount END) AS net_post_genesis
  FROM structs.ledger
 GROUP BY denom
 ORDER BY 2 DESC;

\echo
\echo === GATE 6: unknown_event_log triage (top 20 by count) ===
SELECT composite_key,
       count,
       first_seen_height,
       last_seen_height,
       LEFT(last_payload::text, 60) AS sample_payload
  FROM sync_state.unknown_event_log
 WHERE chain_id = :'chain'
 ORDER BY count DESC LIMIT 20;

\echo
\echo === GATE 6b: unknown_event_log total + bucket by event family ===
SELECT split_part(composite_key, '.', 3) AS event_family,
       COUNT(DISTINCT composite_key) AS distinct_keys,
       SUM(count) AS total_emissions
  FROM sync_state.unknown_event_log
 WHERE chain_id = :'chain'
 GROUP BY 1
 ORDER BY 3 DESC LIMIT 20;

\echo
\echo === GATE 7: handler error fine-grained look ===
SELECT id, height, tx_index, event_index, composite_key, severity,
       LEFT(error, 200) AS error,
       LEFT(payload::text, 200) AS payload_sample
  FROM sync_state.handler_error_log
 WHERE resolved_at IS NULL
 ORDER BY id DESC LIMIT 10;

\echo
\echo === GATE 8: sample RPC round-trip - pick block h=750000 and compare event count ===
SELECT 'sync_state.raw_events @ h=750000' AS what, COUNT(*) AS n
  FROM sync_state.raw_events WHERE chain_id=:'chain' AND height=750000
UNION ALL SELECT 'sync_state.raw_attributes @ h=750000', COUNT(*)
  FROM sync_state.raw_attributes WHERE chain_id=:'chain' AND height=750000
UNION ALL SELECT 'sync_state.raw_tx_results @ h=750000', COUNT(*)
  FROM sync_state.raw_tx_results WHERE chain_id=:'chain' AND height=750000;

\echo
\echo === GATE 9: top heights by raw_attributes density (busy blocks) ===
SELECT height, COUNT(*) AS n_attrs, COUNT(DISTINCT event_index) AS n_events
  FROM sync_state.raw_attributes
 WHERE chain_id=:'chain'
 GROUP BY height ORDER BY 2 DESC LIMIT 10;

\echo
\echo === GATE 10: structs.* state snapshot (what populated) ===
SELECT 'player' AS tbl, COUNT(*) FROM structs.player
UNION ALL SELECT 'player_address', COUNT(*) FROM structs.player_address
UNION ALL SELECT 'planet',         COUNT(*) FROM structs.planet
UNION ALL SELECT 'fleet',          COUNT(*) FROM structs.fleet
UNION ALL SELECT 'struct',         COUNT(*) FROM structs.struct
UNION ALL SELECT 'substation',     COUNT(*) FROM structs.substation
UNION ALL SELECT 'reactor',        COUNT(*) FROM structs.reactor
UNION ALL SELECT 'allocation',     COUNT(*) FROM structs.allocation
UNION ALL SELECT 'guild',          COUNT(*) FROM structs.guild
UNION ALL SELECT 'infusion',       COUNT(*) FROM structs.infusion
UNION ALL SELECT 'agreement',      COUNT(*) FROM structs.agreement
UNION ALL SELECT 'grid',           COUNT(*) FROM structs.grid
UNION ALL SELECT 'planet_attribute', COUNT(*) FROM structs.planet_attribute
UNION ALL SELECT 'struct_attribute', COUNT(*) FROM structs.struct_attribute
UNION ALL SELECT 'permission',     COUNT(*) FROM structs.permission
ORDER BY 1;

\echo
\echo === GATE 11: derived-state cross-check - players with no primary_address (sanity) ===
SELECT 'players missing primary_address' AS what, COUNT(*)
  FROM structs.player WHERE primary_address IS NULL OR primary_address = ''
UNION ALL SELECT 'players whose primary_address has no player_address row',
       COUNT(*)
  FROM structs.player p
  WHERE p.primary_address IS NOT NULL AND p.primary_address <> ''
    AND NOT EXISTS (SELECT 1 FROM structs.player_address pa
                     WHERE pa.player_id = p.id AND pa.address = p.primary_address);

\echo
\echo === GATE 12: doctor drop-list - none of the retired triggers should be enabled ===
SELECT n.nspname AS schema, c.relname AS table, t.tgname AS trigger,
       t.tgenabled::text AS state
  FROM pg_trigger t
  JOIN pg_class c    ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE NOT t.tgisinternal
   AND (
     (n.nspname='structs' AND c.relname='player'           AND t.tgname='update_address_guild_id') OR
     (n.nspname='structs' AND c.relname='planet'           AND t.tgname='name_planet') OR
     (n.nspname='structs' AND c.relname='infusion'         AND t.tgname='add_infusion_ledger_entry') OR
     (n.nspname='structs' AND c.relname='struct'           AND t.tgname='planet_activity_struct_movement') OR
     (n.nspname='structs' AND c.relname='fleet'            AND t.tgname='planet_activity_fleet_move') OR
     (n.nspname='structs' AND c.relname='planet_raid'      AND t.tgname='planet_activity_raid_status') OR
     (n.nspname='structs' AND c.relname='struct_attribute' AND t.tgname='planet_activity_struct_attribute')
   );

\echo
\echo === GATE 12b: player_address_cascade trigger should be PRESENT (intentionally retained) ===
SELECT n.nspname AS schema, c.relname AS table, t.tgname AS trigger, t.tgenabled::text AS state
  FROM pg_trigger t
  JOIN pg_class c    ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE NOT t.tgisinternal
   AND n.nspname='structs'
   AND c.relname='player'
   AND lower(t.tgname) = 'player_address_cascade';
