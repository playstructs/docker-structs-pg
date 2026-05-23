# Postgres persistent volumes

The stack uses two named volumes by default:

| Volume | Mount | Contents |
|--------|--------|----------|
| `pgdata` | `/var/lib/postgresql/` | Cluster data (`…/main/`), WAL, checkpoints |
| `pgetc` | `/etc/postgresql/` | `postgresql.conf`, `pg_hba.conf`, SSL certs, init markers |

## Rules

1. **Never recreate only one volume** on a production deployment. If `pgetc` is wiped while `pgdata` remains (or the reverse), config, SSL, and WAL metadata can diverge and startup may fail with checkpoint or control-file errors.

2. **Back up both volumes together** before major upgrades or image version bumps.

3. **Init markers live on `pgetc`**: `SSL_SETUP`, `SQITCH_INIT_COMPLETE`. Deleting `pgetc` without `pgdata` re-runs SSL setup and may append duplicate `postgresql.conf` lines unless you clean up manually.

## Consolidating to a single volume (optional)

Long term, persisting only `pgdata` and baking static config into the image reduces split-brain risk. That requires a one-time migration:

1. Stop the stack (`docker compose stop structs-pg structs-pg-init`).
2. Back up `pgdata` and `pgetc`.
3. Copy any required files from `pgetc` into the image or a config bind-mount strategy your team chooses.
4. Remove the `pgetc` volume mount from compose and redeploy.

Until you migrate, keep both volumes paired.

## Memory tuning

Set `POSTGRES_MEMORY_MB` (or `POSTGRES_SHARED_BUFFERS`) on `structs-pg` and `structs-pg-init` to match `deploy.resources.limits.memory`. The entrypoint writes `conf.d/structs-memory.conf` at start (~25% of `POSTGRES_MEMORY_MB` for `shared_buffers`).

Do not set `shared_buffers` far above the container memory limit; OOM kills during checkpoint are a common cause of invalid checkpoint records.
