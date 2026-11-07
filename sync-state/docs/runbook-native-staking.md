# Native staking evidence

The v0.22 writer keeps complete, height-pinned native claims in Sqitch-owned
`sync_state.native_staking_snapshot`. This is a current evidence cache, not a
native queue mirror. It stores bonded and actual unbonding totals, and indexes
validator exposure including both sides of an in-flight redelegation.
Redelegations are already included in destination bonded stake; they add no
second asset. Query limits and any failed page leave the address unknown.

The implementation now uses these claims for synthetic inventory and player
valuation in incremental projection, full rebuild, SQL reconciliation and the
verifier. This code has not been deployed to the running writer. Existing browser adapters and rollout rehearsal remain open;
a successful cache refresh alone does not complete the application migration.

## Prerequisites and operation

Deploy through `function-api-player-reactor-20261029` with structs-pg
Sqitch before starting this writer, plus the separately gated ledger identity
switch during the coordinated writer change. Compatible schema predeployment
leaves SQL maintenance on the existing ledger authority. The first writer rebuild
atomically replaces inventory/value and sets the inventory refresh row's
`inventory_authority=current-claims-v1`; old authority forces that rebuild even
at an otherwise current height. Invalidating projections preserves the authority
while clearing readiness. The mandatory schema probe runs even with doctor off.
Revert refuses active authority, unknown claim rows, populated native evidence
or invalidation watermarks; use a verified checkpoint for a coordinated downgrade.

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
`SYNC_STATE_NATIVE_PARALLELISM` / `-native-staking-parallelism` defaults to four
concurrent address fetches, bounded to 1–8. Publication remains serial under the
existing maintenance lock, with stale-height validation and atomic claim refresh.
Successful batches immediately drain the next pending batch. Empty, stale-only
or failed batches pause one second; failed addresses retain their retry backoff.
Shutdown cancels and joins every in-flight fetch. These bound query work and do not
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

Publication replaces totals, exposure and affected inventory/player values in
one transaction under the existing maintenance lock. It emits inventory refresh
hints on existing owner consensus subjects only after commit, including changes
to/from unknown. It does not advance incomplete projection watermarks or fan out
to every guild member/token holder for an individual native query. It refuses future-height, unsolicited, duplicate/older or invalidated results.
A later cursor alone does not require another fetch if no relevant mutation
occurred; a newer change to a discovered validator does. The claim view returns
NULL totals while evidence is incomplete or ahead of its chain's cursor. A
missing row is also unknown. Failed reads never publish partial zeroes, and delayed
Structs defusing cleanup never writes its aggregate into native claim totals.

`ualpha.infused` is current native bonded value plus current generator fuel,
including disabled generators. `ualpha.defusing` is actual native unbonding value;
redelegations add no separate asset. Synthetic ledger receipts remain history and
are excluded from current deltas. Missing/stale/future native evidence yields NULL
claims and NULL aggregate valuation. Real bank balances remain independent and
non-null. An address's query returning complete empty positions proves zero;
a missing query does not. Current economic ownership attributes player/guild
claims, while revoked addresses retain their own unowned claim.

All refresh paths use the same Sqitch-owned current-claim arithmetic. Live refresh
uses indexed current state and no ledger history scan. Full rebuild and scheduled
reconciliation discover historical candidates and aggregate bank receipts; repair
reads fresh authority instead of writing the drift log's older expected value.
`view.guild_inventory` also propagates unknown member claims rather than summing
only known members.

## Diagnostics

Inspect `sync_state.native_staking_claim` for completeness and source height.
Inspect `native_staking_snapshot.pending`, `required_height`, `retry_after` and
`fetched_at` to distinguish unqueried, invalidated and delayed work. Retained old
totals in the snapshot table are diagnostic evidence, not current values while
pending. `native_staking_exposure` and `native_staking_invalidation` explain
validator fan-out and stale-result rejection. The worker logs fetch failures and
keeps unrelated chain ingestion running.

The verifier reports `unknown_claim_addresses` and `unknown_player_values`
separately from projection consistency. PASS proves agreement with authority;
it does not prove every native query completed. It checks all eight model
watermarks and nullable player valuation in one repeatable-read snapshot.

Tests use a disposable Sqitch-deployed PostgreSQL database and mock HTTP gateways
for deterministic complete/partial snapshots. They cover real transaction rollback,
concurrent writer progress during HTTP, stale-result rejection, newly discovered
exposure, source-validator redelegation exposure, exact large totals, empty native
completion, retry isolation and worker shutdown. The release-height chain upgrade,
representative deployment timings and unchanged-UI adapter validation remain
separate acceptance work.

## Pending operation receipts

Deploy Sqitch `table-defusion-20261029-receipts` before this writer. It follows current-claim inventory and precedes the gated ledger switch. The ledger migration also grants indexer USAGE on `structs.ledger_id_seq`; schema validation checks it, and a real restricted-role bank insert exercises it.

`structs.defusion` retains original operation receipts, including retired replay tombstones. New identities are `(chain_id, block_height, tx_index, event_index, denom)` using original RPC coordinates. A native entry ID cannot be derived from this identity. Batches insert idempotently and queue receipt reconciliation only for newly inserted rows. Historical recovery can request another query at the cached height without invalidating otherwise valid balance evidence.

Startup, under the single writer's maintenance lock and sole-chain cursor, records `legacy_observed_height` as an upper source bound on unidentified old rows. It preserves duplicates and leaves their original source coordinates NULL. Never run an old writer alongside this adoption. Native HTTP pagination remains outside the SQL lock; accepted publication reconciles that address's active receipts, current claims and notifications together. Failed/stale/future/partial queries do not retire receipts.

A receipt retires only after the complete native query contains no compatible operation group. Preserve held and zero-balance entries regardless of scheduled completion. SDK unbonds can merge at the same creation height and time; redelegation creation height can instead be the source validator's earlier unbonding height. Events truncate completion times to seconds, while LCD preserves fractional seconds. Matching respects all three distinctions and does not allocate current group balances back to individual receipts.

The old wall-clock `defusion_cleaner` job is removed by Sqitch; its procedure becomes harmless for callers that still invoke it. The replacement is transactionally reconciled retirement, not deletion. Use `structs.defusion_current` for catalog reads; it excludes retired tombstones and reports `pending_state` (`pending`, `unknown`, or `unidentified`). `amount_p` is still the original event amount. Use native current claims for pending balance, never the sum of receipt amounts. Missing or unidentified history is not a complete queue mirror.

`sync-state verify` includes `pending_receipts`: impossible indexed boundaries or missing reconciliation requests fail, unresolved/unidentified coverage is INFO, and matched/retired coverage passes. The ordinary time-ordering check does not claim receipt-lifecycle coverage. Rollback refuses identified/adopted receipts or outstanding receipt work; use a verified checkpoint if that contract must be removed. Do not manually delete tombstones to make rollback pass.

## Per-validator position evidence

Sqitch `table-native-staking-20261029-positions` adds `native_staking_position`
and the parent `positions_complete` flag. The same complete address query now
publishes exact bonded/unbonding totals and actual unbonding-entry counts by
validator in the existing transaction. A held zero-balance entry still occupies
a slot. Redelegations remain exposure, not another asset. The grouped totals
must equal the complete address totals before any mutation is accepted.

Existing snapshots start with `positions_complete=false`, which queues a bounded
refresh even when the old total is current. Same-height publication is permitted
for this missing coverage as well as queued receipt work. Failed queries back off;
complete-empty publication removes prior child rows. `native_staking_position_state`
requires both complete address evidence and complete position coverage. Child rows
alone are diagnostic while the parent is invalidated. Native aggregate claims
remain usable independently of a legacy snapshot's missing position breakdown.

The existing player reactor API uses `structs.api_player_reactor(text)` with the
inventory snapshot. It starts from current player vault/approved retained ownership,
uses exact validator keys and excludes guild treasuries, pending/revoked addresses
and other reactors. At most 1,024 owned addresses may contribute; a 1,025th withholds
the whole result. This read limit is not a transaction message/gas limit. A missing
indexed vault or missing/stale per-address evidence withholds native totals.

Applied infusion power remains a separate canonical field: sum gross power, and
compute the owner share with the release's per-position ties-to-even commission
rounding. Do not reconstruct applied power from native fuel or count cached power
for a different recorded owner. The API exposes exact strings and coverage; it
does not establish current permissions, queue limits, live stake availability,
transaction simulation, or the maximum safe atomic withdrawal size.
