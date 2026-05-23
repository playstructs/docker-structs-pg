\timing on
\pset format aligned
\pset border 2
\set chain 'structstestnet-111'

\echo
\echo === 1a cursor vs tip ===
SELECT chain_id, last_height, tip_height, lag_blocks, status,
       to_char(updated_at, 'YYYY-MM-DD HH24:MI:SS') AS updated
  FROM sync_state.sync_cursor WHERE chain_id = :'chain';

\echo
\echo === 1b block_log coverage (gap detection) ===
WITH bounds AS (
  SELECT MIN(height) AS lo, MAX(height) AS hi, COUNT(*) AS n
    FROM sync_state.block_log WHERE chain_id = :'chain'
)
SELECT lo, hi, n, (hi - lo + 1) AS expected, (hi - lo + 1) - n AS gaps FROM bounds;

\echo
\echo === 1c handler errors (by composite_key + severity) ===
SELECT composite_key, severity, COUNT(*) AS n
  FROM sync_state.handler_error_log
 WHERE resolved_at IS NULL
 GROUP BY composite_key, severity
 ORDER BY 3 DESC;

\echo
\echo === 1d genesis loaded invariant ===
SELECT gl.chain_id, gl.applied_at::timestamp(0), gl.total_rows AS log_says,
       (SELECT COUNT(*) FROM structs.ledger WHERE action='genesis') AS ledger_has,
       CASE WHEN gl.total_rows = (SELECT COUNT(*) FROM structs.ledger WHERE action='genesis')
            THEN 'PASS' ELSE 'FAIL' END AS verdict
  FROM sync_state.genesis_log gl WHERE gl.chain_id = :'chain';

\echo
\echo === 1e raw_mirror coverage (raw_blocks vs block_log) ===
SELECT
  (SELECT COUNT(*) FROM sync_state.raw_blocks WHERE chain_id=:'chain') AS raw_blocks,
  (SELECT COUNT(*) FROM sync_state.block_log  WHERE chain_id=:'chain') AS block_log,
  CASE WHEN (SELECT COUNT(*) FROM sync_state.raw_blocks WHERE chain_id=:'chain')
          = (SELECT COUNT(*) FROM sync_state.block_log  WHERE chain_id=:'chain')
       THEN 'PASS' ELSE 'WARN' END AS verdict;

\echo
\echo === 1f current_block status (webapp-facing heartbeat) ===
SELECT chain_id, height, status, lag_blocks, tip_height,
       to_char(updated_at, 'YYYY-MM-DD HH24:MI:SS') AS updated
  FROM structs.current_block ORDER BY updated_at DESC LIMIT 3;
