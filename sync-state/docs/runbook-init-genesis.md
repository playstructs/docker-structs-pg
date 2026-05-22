# Operator runbook: `sync-state init-genesis`

> Related runbooks:
> [`runbook-ownership-flags.md`](runbook-ownership-flags.md) (what sync-state owns),
> [`runbook-verify-and-replay.md`](runbook-verify-and-replay.md) (the `genesis_loaded` check),
> [`runbook-rpc-failover.md`](runbook-rpc-failover.md) (primary/seed RPC pool).

`init-genesis` ports docker-structsd's `scripts/indexer-insert-genesis.sh`
into Go and folds it into `sync-state`. It loads the chain's genesis
JSON and seeds `structs.ledger` with the bank balances, staking
positions, and player-ore balances that exist at block 0 — the rows
every later block's debits assume are already present.

Without it, ingest will eventually log rows like:

```text
- per (address, denom) net balance using direction:
+---------------+
| rows_negative |
+---------------+
|             9 |
+---------------+
```

…because the chain emits debit events for accounts whose genesis credit
was never imported.

---

## TL;DR

The default workflow auto-applies on first ingest, so you don't need to
do anything explicit:

```shell
sync-state bootstrap         # DDL + schema migration (idempotent)
sync-state doctor            # confirm node + DB look healthy
sync-state ingest            # auto-runs init-genesis on first start from h=1
```

When the first ingest sees `start-height=1` and no `sync_state.genesis_log`
row, it loads + applies genesis before fetching block 1.

To do it explicitly (e.g. air-gapped import, or operators wanting tight
control), pass `-genesis-auto-apply=false` to ingest and run init-genesis
yourself:

```shell
sync-state bootstrap
sync-state init-genesis
sync-state doctor
sync-state ingest -genesis-auto-apply=false
```

---

## What it imports

Four sections of the genesis JSON, all landed in `structs.ledger` with
`action='genesis'`:

| Source | Denom | Rows per entry | Notes |
|--------|-------|----------------|-------|
| `app_state.bank.balances[].coins[]` | per-coin | 1 (credit) | One row per (address, denom). |
| `app_state.staking.delegations[]` | `ualpha.infused` | 2 (credit×2) | Delegator + validator side. Amount = `floor(shares × validator_tokens / validator_shares)`. |
| `app_state.staking.unbonding_delegations[].entries[]` | `ualpha.defusing` | 2 (credit×2) | Same two-row shape as delegations. |
| `app_state.structs.gridList[]` where `attributeId LIKE '0-1-%' AND value <> '0'` | `ore` | 1 (credit) | Player index resolved via `playerList[]` → `primaryAddress`. |

`block_height=0` and `time=<genesis_time>` on every row, matching the
shell script byte-for-byte.

---

## Source selection: RPC vs file

Default is **RPC** (`/genesis` with `/genesis_chunked` fallback for files
that exceed `max_body_bytes`). For air-gapped imports or chains whose
node config disables `/genesis`, pass `-genesis-file`:

```shell
sync-state init-genesis -genesis-file=/var/lib/structsd/config/genesis.json
```

Even with `-genesis-file`, the subcommand still tries RPC `/status` to
get the canonical chain_id and refuses to import a mismatched genesis
(belt-and-braces against a mainnet genesis aimed at a testnet DB). The
mismatch check is skipped only when both `/status` is unreachable AND
`-genesis-file` is set (fully offline).

---

## Quick recipes

```shell
# Default: fetch via RPC, apply if no genesis_log row exists for this chain.
sync-state init-genesis

# Force re-apply (DELETE FROM structs.ledger WHERE action='genesis' first).
sync-state init-genesis -force

# Import from local file instead of RPC.
sync-state init-genesis -genesis-file=/path/to/genesis.json

# Combine: re-import from a hand-edited local file.
sync-state init-genesis -genesis-file=/path/to/genesis.json -force
```

---

## Exit codes

| Code | Meaning |
|------|---------|
| 0    | Genesis applied, OR was already applied (no `-force`) — both states are healthy. |
| 1    | Refused (chain_id mismatch), or RPC/DB error, or apply transaction failed. |
| 2    | Invalid args. |

---

## Knobs

| Flag | Default | Notes |
|------|---------|-------|
| `-genesis-file` | `""` (RPC) | Local JSON path. Set when the node's `/genesis` is disabled or for air-gapped imports. |
| `-force` | `false` | Re-apply even when `genesis_log` already records a row. Always safe (the apply tx wipes prior `action='genesis'` rows first), but the default refusal protects against an accidental re-run during a chain upgrade. |
| `-genesis-auto-apply` | `true` | Ingest-only knob. Set false on ingest invocation to disable the auto-load path; init-genesis must then be run explicitly. |

Env equivalents:

```text
SYNC_STATE_GENESIS_FILE         -> -genesis-file
SYNC_STATE_GENESIS_FORCE        -> -force
SYNC_STATE_GENESIS_AUTO_APPLY   -> -genesis-auto-apply
```

---

## What gets recorded

`sync_state.genesis_log` (one row per chain):

```text
chain_id           VARCHAR PRIMARY KEY
applied_at         TIMESTAMPTZ
source             TEXT          -- "rpc:<url>" or "file:<path>"
genesis_time       TIMESTAMPTZ   -- from the genesis doc itself
sha256             VARCHAR(64)   -- over the raw JSON bytes (pre-parse)
rows_per_section   JSONB         -- {"bank":138,"delegations":40,"unbondings":2,"ore":7}
total_rows         BIGINT
```

Inspect:

```sql
SELECT chain_id, applied_at, source, genesis_time, total_rows, rows_per_section
  FROM sync_state.genesis_log;
```

The `sha256` is over the raw genesis JSON pre-parse, so a re-apply with
a tampered file is detectable by comparing hashes — you'll see a new
`applied_at` and a different `sha256` for the same `chain_id`.

---

## Replay safety

`Apply()` runs in a single transaction with the following order:

1. `DELETE FROM structs.ledger WHERE action='genesis'`
2. INSERT bank balances (one batched multi-row INSERT)
3. INSERT delegations (one batched multi-row INSERT)
4. INSERT unbondings (one batched multi-row INSERT)
5. INSERT player ore (one batched multi-row INSERT)
6. UPSERT `sync_state.genesis_log` row

If any step fails the whole tx rolls back, leaving the pre-existing
genesis rows (if any) intact. Re-running init-genesis is always safe.

The `structs.ledger` schema has no `chain_id` column (single-chain DB by
design — same convention as docker-structsd's shell script), so the
DELETE in step 1 is a global wipe of the `action='genesis'` tag. The
`genesis_log` row is the single source of truth for "is genesis loaded?".

---

## Verifying success

After applying genesis, `sync-state verify` runs `genesis_loaded`:

```text
genesis_loaded                   PASS  genesis applied (188 rows from rpc:http://reactor.oh.energy:26657)
    applied_at = 2026-05-21T19:31:01Z
    genesis_time = 2026-03-26T12:27:27Z
    rows_per_section = map[bank:138 delegations:40 ore:8 unbondings:2]
    sha256 = c8a4...
    source = rpc:http://reactor.oh.energy:26657
    total_rows = 188
```

And the per-address negative-balance audit query goes to zero:

```sql
WITH bal AS (
  SELECT address, denom,
         SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END) AS net
    FROM structs.ledger
   GROUP BY address, denom
)
SELECT COUNT(*) AS rows_negative FROM bal WHERE net < 0;
```

A non-zero `rows_negative` post-init-genesis is a real bug — either a
handler is emitting debits without the corresponding credits, or the
genesis you imported is missing entries the chain assumes exist.

---

## Failure modes

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `init-genesis: rpc /status: ...` | RPC unreachable AND no `-genesis-file` set | Provide `-genesis-file`, or fix RPC, or wait for the node to come up. |
| `init-genesis: REFUSING: genesis chain_id=... != RPC chain_id=...` | Mismatched genesis file vs node | Confirm the file matches the chain you want to import. |
| `init-genesis: already applied for chain=... Pass -force to re-apply` | `genesis_log` row exists | Pass `-force` if you really do want to re-apply (rare; only needed if you suspect the import was tampered with). |
| `init-genesis: apply: delegation X->Y: shares not a number: "..."` | Malformed genesis (shouldn't happen with chain-produced files) | Investigate the genesis doc; do not bypass — silently importing garbage produces hard-to-find ledger bugs. |
| `init-genesis: apply: ERROR: duplicate key value violates unique constraint ...` | Manual rows in `structs.ledger` with `action='genesis'` collided with the apply | Rare — the apply path deletes priors first. If you see this, file an issue with the SQL log. |

---

## Auto-apply behaviour (ingest)

`runIngest` reads `sync_state.genesis_log` on startup and branches:

1. **Row present** → log "Genesis: already applied at ..." and proceed.
2. **No row, `start <= 1`, auto-apply on** → load + apply genesis, then
   proceed.
3. **No row, `start > 1`** (resuming from a non-genesis height) → loud
   `WARN`, proceed anyway. `verify -errors-only` and `verify` will both
   surface this state; the syncer doesn't refuse to start because some
   archive-node re-syncs legitimately have genesis pre-loaded
   out-of-band.
4. **No row, auto-apply off** → same loud WARN as (3). Run
   `sync-state init-genesis` to silence it.

The branch chosen is logged to stderr on every ingest start so you can
tell from a log scrape whether the auto-load fired.
