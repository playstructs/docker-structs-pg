\timing on
\pset format aligned
\pset border 2
\set chain 'structstestnet-111'

\echo
\echo === 1f current_block status (webapp-facing heartbeat) ===
SELECT chain, height, status, lag_blocks, tip_height,
       to_char(updated_at, 'YYYY-MM-DD HH24:MI:SS') AS updated
  FROM structs.current_block ORDER BY updated_at DESC LIMIT 3;

\echo
\echo === GATE 2: column populated-ness (block_height NULLs across append-only tables) ===
SELECT 'planet_activity' AS tbl, COUNT(*) FILTER (WHERE block_height IS NULL) AS null_h,
                                 COUNT(*) AS total
  FROM structs.planet_activity
UNION ALL SELECT 'ledger',                   COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.ledger
UNION ALL SELECT 'stat_ore',                 COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_ore
UNION ALL SELECT 'stat_fuel',                COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_fuel
UNION ALL SELECT 'stat_capacity',            COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_capacity
UNION ALL SELECT 'stat_load',                COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_load
UNION ALL SELECT 'stat_power',               COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_power
UNION ALL SELECT 'stat_structs_load',        COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_structs_load
UNION ALL SELECT 'stat_connection_capacity', COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_connection_capacity
UNION ALL SELECT 'stat_connection_count',    COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_connection_count
UNION ALL SELECT 'stat_struct_health',       COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_struct_health
UNION ALL SELECT 'stat_struct_status',       COUNT(*) FILTER (WHERE block_height IS NULL), COUNT(*) FROM structs.stat_struct_status
ORDER BY 1;

\echo
\echo === GATE 2: planet_activity_sequence consistency ===
SELECT 'pa.seq > pas.counter' AS what,
       COUNT(*) AS n
  FROM structs.planet_activity pa
  JOIN structs.planet_activity_sequence pas USING (planet_id)
 WHERE pa.seq > pas.counter
UNION ALL
SELECT 'duplicate (planet_id, seq)',
       COUNT(*)
  FROM (SELECT planet_id, seq FROM structs.planet_activity
         GROUP BY 1,2 HAVING COUNT(*) > 1) d;

\echo
\echo === GATE 3: ordered_timeseries_monotonic (per-table timing) ===

\echo --- ledger (PARTITION BY address, denom ORDER BY time, block_height) ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY address, denom ORDER BY time, block_height) AS prev_h
    FROM structs.ledger
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- planet_activity (PARTITION BY planet_id ORDER BY time, seq) ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY planet_id ORDER BY time, seq) AS prev_h
    FROM structs.planet_activity
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_ore (PARTITION BY object_type, object_index ORDER BY time, block_height) ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_type, object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_ore
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_fuel ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_type, object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_fuel
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_power ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_type, object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_power
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_capacity ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_type, object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_capacity
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_load ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_type, object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_load
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_structs_load ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_structs_load
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_connection_capacity ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_connection_capacity
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_connection_count ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_connection_count
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_struct_health ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_struct_health
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo --- stat_struct_status ---
SELECT COUNT(*) AS inversions FROM (
  SELECT block_height, LAG(block_height) OVER (PARTITION BY object_index ORDER BY time, block_height) AS prev_h
    FROM structs.stat_struct_status
) inv WHERE prev_h IS NOT NULL AND block_height < prev_h;

\echo
\echo === GATE 4: ledger balance sanity (per-address/denom net) ===
WITH bal AS (
  SELECT address, denom,
         SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END) AS net
    FROM structs.ledger
   GROUP BY address, denom
)
SELECT COUNT(*) FILTER (WHERE net < 0) AS negative_balances,
       COUNT(*) FILTER (WHERE net = 0) AS zero_balances,
       COUNT(*) FILTER (WHERE net > 0) AS positive_balances,
       COUNT(*) AS total_pairs
  FROM bal;

\echo
\echo --- if any negative, show them ---
WITH bal AS (
  SELECT address, denom,
         SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END) AS net
    FROM structs.ledger
   GROUP BY address, denom
)
SELECT * FROM bal WHERE net < 0 ORDER BY net LIMIT 25;
