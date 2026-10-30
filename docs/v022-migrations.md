# v0.22 migration packaging and recovery

The image bundles one exact structs-pg commit, recorded by `STRUCTS_PG_REF`
and the `io.structs.pg.revision` image label. Publish that commit before the
image build. A branch name or a moving tag is not a valid build pin.

The current pin also includes issued device activation identities required by
the updated webapp. This additive change binds pending consent to a UUID instead
of only the reusable short code, and removes that consent when its code expires
or is deleted. It remains after the coordinated transition in the Sqitch plan;
the compatible automatic target below is unchanged. Do not deploy the new webapp
against a schema that ends before `table-player-address-20261030-activation-instance`.

The writer also requires `table-work-20261030-uint64`. Attribute values, work
clocks and the three catalog difficulties retain the full unsigned chain range;
startup refuses the former integer storage. The migration preserves view grants
and work dirty-trigger settings. Keep it in the coordinated pause: the type
change takes table locks and is not a rollout for the old SQL cache writer.
Reversal refuses stored values above the former signed integer range. No new
container activation or wider automatic target is implied by this build pin.

```sh
docker build --build-arg STRUCTS_PG_REF=<full-tested-commit> -t structs/structs-pg:<candidate-tag> .
```

Building a candidate does not authorize a database deployment. The full switch
also requires the matching v0.22 TSA worker and application transaction code.

## Automatic compatible deployment

Both init and auto-migrate use `scripts/migrations-common.sh`. Defaults are:

| Variable | Default | Meaning |
|---|---|---|
| `STRUCTS_PG_MIGRATION_MODE` | `pinned` | Use the exact bundled commit; do not pull main |
| `STRUCTS_PG_REF` | Image build pin | Full 40-character commit SHA |
| `STRUCTS_PG_MIGRATION_TARGET` | `table-signer-tx-20261030-authority-contract` | Stop at the tested compatible boundary |
| `STRUCTS_PG_BRANCH` | `origin` | Legacy Compose sentinel; accepted in pinned mode |
| `STRUCTS_LEDGER_WRITER_CONTRACT` | Unset | Explicit `denomination-v1` for the coordinated ledger switch |
| `STRUCTS_SIGNER_CONTRACT` | Unset | Explicit `v022-1` for the coordinated signer switch |

The recurring migrator remains enabled for its configured target. If the target
is already deployed, including when the database is beyond it, the migrator
verifies that change instead of attempting a backwards deployment. Git, registry
connection, Sqitch deployment and verification errors terminate the process;
container restart policy can retry them. No failure is treated as success.

The previous container runs unbounded `sqitch deploy`. If it pulls this schema
before replacement, the ledger gate refuses the incompatible transition, but
Sqitch's default `--mode all` also reverts every change applied in that run.
The compatible prefix does not remain deployed. Its old loop then retries the
same deploy/revert cycle. Stop that migrator and use the target-bounded command;
do not enable transition gates or change rollback mode to make the error disappear.

For a deliberate development workflow, `STRUCTS_PG_MIGRATION_MODE=branch` plus
an explicit `STRUCTS_PG_BRANCH` tracks that remote branch. It still respects the
configured migration target. Release deployments should keep pinned mode.

## Coordinated activation

Keep application submissions and the old writer stopped during this switch.
Run the one-shot migrator with the final approved image, preserving its database
connection configuration, and these explicit settings:

```sh
STRUCTS_PG_MIGRATION_TARGET=latest \
STRUCTS_LEDGER_WRITER_CONTRACT=denomination-v1 \
STRUCTS_SIGNER_CONTRACT=v022-1 \
/src/scripts/database-migrate.sh
```

The settings acknowledge the paired writer and signer transition; they do not
stop any services. The old TSA must not run against the activated signer
contract. Sqitch itself enforces both transition gates and DDL lock timeouts.
The new writer refuses a schema without the source-coverage migration.

## Existing database already indexed the upgrade block

The observed local testnet cursor was exactly 3,353,000, but the old writer had
ignored vault/diversity events and omitted additional coins in multi-denomination
transfers. A current cursor is therefore insufficient evidence of completeness.
After the coordinated schema switch, while the ordinary writer remains stopped:

```sh
sync-state reconcile-current-block -start=3353000 -mirror-raw=true
```

Use the image's normal RPC and database environment. The command acquires the
writer and projection-maintenance locks, fetches the complete source block, and
requires the indexed height, hash and timestamp to match. It does not rewind or
replay an arbitrary historical range over newer state.

For legacy infusion positions with an unknown phase, the retained coordinates
must identify the final mutation in the complete execution order. Stored legacy
fields must match that snapshot, or a deletion must already be absent. Only then
are the phase and previously unhandled extension fields adopted; no duplicate
fuel delta is emitted. Unmatched positions or conflicting snapshots refuse the
repair. Existing ledger identities must agree on their full financial values;
missing denomination legs can then be inserted without replacing history.

Source repair, the v0.22 coverage watermark, API projection rebuilding, raw
source mirroring and the cursor commit together. Repeating the command at the
same boundary is safe. A failure rolls the entire repair back. Native staking
snapshots still need their normal height-pinned refresh before affected actions
can use them; this command does not invent those balances.

`sync_state.economy_contract` records contiguous complete-block coverage starting
at the activation height. `structs.api_readiness` requires that record to match
the indexed height and hash after activation. A projection rebuild alone cannot
make an old writer's data ready, and an old writer advancing the cursor cannot
carry the new readiness watermark forward. Ingest startup refuses that state
before rebuilding projections.

If the cursor has advanced beyond the upgrade without coverage, restore a
verified complete database checkpoint at or before the activation boundary and
catch up with the new writer. Preserve newer off-chain signer/application state
as specified in the coordinated recovery runbook. Do not delete provenance,
guess phases, lower the cursor alone, or use `--allow-replay` as a substitute.

## Validation

`python3 tests/test_migrations.py` exercises pinning, branch opt-in, compatible
targets, explicit gates and failure propagation against temporary Git repos.
The Go integration suite runs against the Sqitch schema. The captured testnet
upgrade replay test additionally requires a marked disposable restore through
`V022_UPGRADE_REHEARSAL_DATABASE_URL`; ordinary integration runs skip it.

`python3 tests/test_migrations_integration.py --image sha256:<local-image-id>`
tests the actual packaged Sqitch and migration helper in a temporary database
container with no network. It reproduces the legacy gate-triggered full rollback,
then verifies compatible-target deployment, unchanged ledger identity and a second
verification-only run. The temporary container is removed afterward.
