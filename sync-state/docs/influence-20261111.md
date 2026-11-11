# Influence ingestion deployment

This writer requires structs-pg `influence-schema-20261111` and
`influence-projector-20261111`. Startup schema validation and doctor enforce the
contract. Deploy/verify both migrations before starting this writer. The Dockerfile
pins the matching structs-pg revision and checks that both migrations are included.
Publish that schema commit before building the image, since the build fetches its
pinned revision from GitHub. Deploy the schema and writer before registering a
contest; registration requires verified influence source coverage. No production
migration or restart is performed by this implementation.

The existing image/live deployment has SUPERUSER application roles. New influence
ACLs are tested with non-superuser roles, but require a separate grant audit and
role hardening before those boundaries can be enforced in that deployment. Sqitch
verify warns explicitly; this change does not alter global application roles.

The canonical schema, scoring, SQL administration, chart-read and recovery
contracts are in [structs-pg's influence documentation](../../../structs-pg/docs/influence-20261111.md)
(sibling checkout). Default points are 100 per guild-covered planet and 200 per
maintained claim, with no ore/defense points. All source attribution uses the
defending planet and event-time ownership/guild membership.

Both streaming and bulk ingestion call the same tracker inside the complete
block/window transaction. Keep the existing single-writer lock. Failed
transaction gameplay events cannot alter base ownership/membership or influence.
The tracker pairs successful raids with preceding theft evidence in the same
transaction/message; missing, ambiguous or inconsistent evidence stops ingestion
instead of guessing. Even unchanged raid statuses produce new canonical facts.
Influence updates are not deferred when ordinary API projections are deferred.

At cutover, exact influence coverage starts at the next verified current-state
boundary. Earlier history is **not backfilled from today's guilds or ownership**.
It needs a historical canonical snapshot plus complete ordered block ingestion.
Contests starting before exact coverage are rejected. Monitor `sync-state doctor`
for coverage/canonical-height disagreement and unassigned sectors. Schema
validation still runs when RPC doctor checks are disabled.

Create/register contests using owner SQL after capture starts. Empty blocks still
advance their watermark and finalize them at the inclusive end. Future gameplay
cannot change final results. Internal reconciliation is available via
`structs.influence_reconcile(build_id)`, and owner-only
`structs.influence_rebuild(contest_id)` republishes an independently rebuilt and
reconciled generation. Large rebuilds hold the influence writer lock.

For charts, run the bounded maintenance command with the **view owner's** DSN:

```sh
sync-state influence-refresh
```

Configure `DATABASE_URL` using the existing credential mechanism. The command
requires no RPC. It refreshes at most 10,000 heights per published build, then
atomically advances that build's rollup watermark/checkpoint. Run periodically
or repeat to catch up. It deliberately has no automatic scheduling or raw-data
retention policy. API reads can always use raw history beyond the verified marker.

`reprocess-errors` cannot commit isolated old handlers once influence capture
exists; this would bypass historical attribution. Use complete-block recovery.
The existing general `rewind` and `replay` CLI stubs are not implemented by this
change. The schema's influence-only rewind requires canonical base tables and
normal sync cursors already restored to the target. Follow the schema runbook;
do not use it to rewind only points on top of current ownership.

## Tests

```sh
# From docker-structs-pg (local image; no live database connections):
python3 tests/test_influence_integration.py --image structs/structs-pg:latest

# From sync-state:
go test ./...
go build ./cmd/sync-state

# Against a disposable, fully Sqitch-migrated game DB:
# set INTEGRATION_DATABASE_URL using your local test credential mechanism
go test ./internal/sync -run 'TestInfluenceActualBlock|TestRequiredFailure|TestNativeInvalidation_ActualBlock' -v
```

The standalone runner uses minimal entity tables for fast, isolated scoring
tests; it is not a replacement for the full-schema block tests. Its maintenance
test commits temporary fixtures only in its disposable database/container.
No HTTP endpoints are added in this change. structs-webapp can next consume the
published standings, planet support/claims and exact history read contract.
