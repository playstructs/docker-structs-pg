# v0.22 migration packaging and recovery

The image bundles one exact structs-pg commit, recorded by `STRUCTS_PG_REF`
and the `io.structs.pg.revision` image label. Publish that commit before the
image build. A branch name or a moving tag is not a valid build pin.

The current pin also includes issued device activation identities required by
the updated webapp. This additive change binds pending consent to a UUID instead
of only the reusable short code, and removes that consent when its code expires
or is deleted. It remains after the coordinated transition in the Sqitch plan;
fresh installations deploy it automatically. Do not deploy the new webapp
against a schema that ends before `table-player-address-20261030-activation-instance`.

The writer also requires `table-work-20261030-uint64`. Attribute values, work
clocks and the three catalog difficulties retain the full unsigned chain range;
startup refuses the former integer storage. The migration preserves view grants
and work dirty-trigger settings. Keep it in the coordinated pause: the type
change takes table locks and is not a rollout for the old SQL cache writer.
Reversal refuses stored values above the former signed integer range. The build
pin records the bundled schema; runtime migration selection is configured
separately below.

The webapp also needs `role-structs-webapp-20261030-gameplay-reads` for existing
raid, shield, activity, catalog and statistics reads when its database role is
not a superuser. This grants SELECT on the required relations only; it does not
alter role attributes or permit writes to indexed chain state. Verification
checks explicit ACLs on legacy superusers as well as effective restricted-role
access.

```sh
docker build --build-arg STRUCTS_PG_REF=<full-tested-commit> -t structs/structs-pg:<candidate-tag> .
```

Building a candidate does not authorize a database deployment. The full switch
also requires the matching v0.22 TSA worker and application transaction code.

## Automatic deployment from main

Both init and auto-migrate use `scripts/migrations-common.sh`. Defaults are:

| Variable | Default | Meaning |
|---|---|---|
| `STRUCTS_PG_MIGRATION_MODE` | `main` | Fetch remote main on every run |
| `STRUCTS_PG_REF` | Image build pin | Bundled full commit SHA; used at runtime only in pinned mode |
| `STRUCTS_PG_MIGRATION_TARGET` | `latest` | Deploy and verify all pending Sqitch changes |
| `STRUCTS_PG_BRANCH` | `origin` | Legacy Compose sentinel for main; `main` also works |
| `STRUCTS_LEDGER_WRITER_CONTRACT` | Unset | Explicit `denomination-v1` for an existing database's coordinated ledger switch |
| `STRUCTS_SIGNER_CONTRACT` | Unset | Explicit `v022-1` for an existing database's coordinated signer switch |

New migrations pushed to main are applied on the next recurring run (every 120
seconds). A fresh installation accepts the existing `STRUCTS_PG_BRANCH=main`
configuration without another mode setting. An explicit branch name overrides
main; `STRUCTS_PG_MIGRATION_MODE=branch` remains supported and requires a name.
Git fetch failures stop the run rather than deploying a stale checkout.

Fresh databases initialize through the latest change automatically. The helper
checks that there are no relations in the `structs` schema and no applied Sqitch
changes before supplying the v0.22 writer/signer contracts. It does not trust a
volume initialization marker. Existing databases still require the coordinated
transition below if those guarded changes have not been applied. Databases that
already completed that transition accept subsequent migrations normally.

Explicit settings still override these defaults. Remove an old migration target
or set `STRUCTS_PG_MIGRATION_TARGET=latest` to follow all changes. The guild Compose
file must also default its target to `latest`; an older Compose file's explicit
boundary overrides the image. No running deployment is changed by an image build.

For reproducible staging, select `STRUCTS_PG_MIGRATION_MODE=pinned`, keep
`STRUCTS_PG_BRANCH=origin` (or unset), and set `STRUCTS_PG_REF` to a full SHA.
A named `STRUCTS_PG_MIGRATION_TARGET` can deliberately stop at a reviewed change.
If that change is already deployed, including when the database is beyond it,
the migrator verifies it instead of attempting a backwards deployment.

Git, registry connection, Sqitch deployment and verification errors terminate
the process; container restart policy can retry them. No failure is treated as
success. A guarded transition failure uses Sqitch's normal rollback behavior;
stop an old deployment's migrator and coordinate its writer/signer switch instead
of bypassing the guards.

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

`python3 tests/test_migrations.py` exercises automatic main updates, pinning,
explicit branches, fresh initialization, bounded targets, explicit gates and
failure propagation against temporary Git repos.
The Go integration suite runs against the Sqitch schema. The captured testnet
upgrade replay test additionally requires a marked disposable restore through
`V022_UPGRADE_REHEARSAL_DATABASE_URL`; ordinary integration runs skip it.

`python3 tests/test_migrations_integration.py --image sha256:<local-image-id>`
tests the actual packaged Sqitch and migration helper in a temporary database
container with no network. It reproduces the legacy gate-triggered full rollback,
then verifies compatible-target deployment, unchanged ledger identity and a second
verification-only run. It also initializes a fresh database through the plan tip
without explicit transition contracts and repeats the migration. Use
`--scripts-dir scripts` to test local helpers against the image's PostgreSQL and
Sqitch installation. The temporary container is removed afterward.
