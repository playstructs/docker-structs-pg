-- One-time repair: remove spurious 'received' credits that duplicate ore-refinery
-- 'refined' credits on the same player/tx.
--
-- MsgStructOreRefineryComplete emits EventAlphaRefine (handled by sync-state)
-- AND a Cosmos transfer pool→player (handled by bank.ProcessBlock). Before
-- the fix in internal/bank/process.go, both credited the player's liquid
-- ualpha.
--
-- Run manually after deploying the sync-state fix. Safe to re-run (0 rows
-- deleted when nothing matches).

BEGIN;

DELETE FROM structs.ledger rc
USING structs.ledger r
WHERE rc.action = 'received'
  AND rc.direction = 'credit'
  AND rc.denom = 'ualpha'
  AND r.action = 'refined'
  AND r.direction = 'credit'
  AND r.denom = 'ualpha'
  AND rc.block_height = r.block_height
  AND rc.address = r.address
  AND rc.time = r.time
  AND rc.amount_p = r.amount_p;

-- Sanity: no negative nets after repair (informational).
SELECT address, denom, net
FROM (
  SELECT address, denom,
         SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END) AS net
  FROM structs.ledger
  GROUP BY address, denom
) b
WHERE net < 0
ORDER BY address, denom;

COMMIT;
