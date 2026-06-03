-- One-time repair: remove spurious 'sent' debits that duplicate struct
-- infusion 'infused' debits on the same player/tx.
--
-- MsgStructGeneratorInfuse emits EventInfusion (handled by sync-state) AND
-- a Cosmos transfer (handled by bank.ProcessBlock). Before the fix in
-- internal/bank/process.go, both debited the player's liquid ualpha.
--
-- Run manually after deploying the sync-state fix. Safe to re-run (0 rows
-- deleted when nothing matches).

BEGIN;

DELETE FROM structs.ledger s
USING structs.ledger i
WHERE s.action = 'sent'
  AND s.direction = 'debit'
  AND s.denom = 'ualpha'
  AND i.action = 'infused'
  AND i.direction = 'debit'
  AND i.denom = 'ualpha'
  AND s.block_height = i.block_height
  AND s.address = i.address
  AND s.time = i.time
  AND s.amount_p = i.amount_p;

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
