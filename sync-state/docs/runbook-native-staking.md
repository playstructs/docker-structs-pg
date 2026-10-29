# Native staking evidence

The v0.22 writer keeps complete, height-pinned native claims in Sqitch-owned
`sync_state.native_staking_snapshot`. This is a current evidence cache, not a
native queue mirror. It stores bonded and actual unbonding totals, and indexes
validator exposure including both sides of an in-flight redelegation.
Redelegations are already included in destination bonded stake; they add no
second asset. Query limits and any failed page leave the address unknown.

This slice does **not yet replace synthetic inventory/leaderboard valuation**.
That activation must use this authority consistently in incremental projection,
full rebuild, reconciliation, verification and existing API adapters. Do not
interpret successful cache refresh as completion of the application migration.

## Prerequisites and operation

Deploy `table-native-staking-20261029-snapshots` through structs-pg Sqitch before
starting this writer. It is additive and precedes the separate gated ledger
identity switch. The mandatory schema probe checks it even with doctor disabled.
The migration provides deploy, verify and guarded revert; populated evidence or
invalidation watermarks cannot be dropped by revert.

`SYNC_NATIVE_STAKING_REFRESH` / `-native-staking-refresh` defaults to true and
uses `STRUCTS_API_URL` / `-lcd`. The worker belongs to the existing ingest process
and joins on shutdown. It fetches HTTP outside the database maintenance lock.
Disabling it leaves new/invalidated claims unknown; it does not disable chain
ingestion, establish zero holdings or authorize a fallback to historical sums.
One-shot ingestion may finish with pending claims; its exit is not their readiness
signal.

Startup seeds previously observed/owned native candidates once from current
ownership, reactor infusions and address inventory. Inventory values are never
used as claim evidence. Later ownership and staking events add candidates inside
their canonical transaction. A liquid transfer alone does not invalidate already
verified native evidence.

Each background cycle selects at most eight due addresses using the partial
pending-work index once the cursor reports current (at most one block behind).
Catch-up records invalidations without repeatedly querying historical/pruned
positions. Queries use 100 rows per page, with limits of 1,024 pairs and
8,192 entries for one complete address. The cycle has a ten-second HTTP budget;
SQL publication/retry gets a separate five-second bound. Failed attempts back
off for thirty seconds, while a newer chain invalidation removes that delay.
There is a one-second pause between cycles. These bound query work and do not
set transaction or withdrawal limits. An account exceeding a bound remains
unknown until the bounds are deliberately revised and its full query succeeds.

## Consistency

Native events, reactor infusion updates/deletes, commission revision events and
the confirmed upgrade boundary invalidate evidence in the same transaction as
canonical state and the cursor. Empty native completion events still invalidate
entries even though they produce no ledger payment. Required invalidation failures
roll back the complete block/window. Surgical Structs-event recovery carries its
native invalidation through the same path.

Validator pool changes invalidate indexed exposed addresses. Financial `slash`
events carry consensus identities while the existing reactor table stores
operator identities, so those events conservatively invalidate every known
exposed account. Jail-only events do not. Validator/global change heights are
retained even when no exposed account is indexed yet: a query that discovers new
exposure must also pass those watermarks before publication.

Publication replaces totals and exposure together under the existing maintenance
lock. It refuses future-height, unsolicited, duplicate/older or invalidated results.
A later cursor alone does not require another fetch if no relevant mutation
occurred; a newer change to a discovered validator does. The claim view returns
NULL totals while evidence is incomplete or ahead of its chain's cursor. A
missing row is also unknown. Failed reads never publish partial zeroes, and delayed
Structs defusing cleanup never writes its aggregate into native claim totals.

## Diagnostics

Inspect `sync_state.native_staking_claim` for completeness and source height.
Inspect `native_staking_snapshot.pending`, `required_height`, `retry_after` and
`fetched_at` to distinguish unqueried, invalidated and delayed work. Retained old
totals in the snapshot table are diagnostic evidence, not current values while
pending. `native_staking_exposure` and `native_staking_invalidation` explain
validator fan-out and stale-result rejection. The worker logs fetch failures and
keeps unrelated chain ingestion running.

Tests use a disposable Sqitch-deployed PostgreSQL database and mock HTTP gateways
for deterministic complete/partial snapshots. They cover real transaction rollback,
concurrent writer progress during HTTP, stale-result rejection, newly discovered
exposure, source-validator redelegation exposure, exact large totals, empty native
completion, retry isolation and worker shutdown. The release-height chain upgrade,
inventory authority transition and representative deployment timings remain separate
acceptance work.
