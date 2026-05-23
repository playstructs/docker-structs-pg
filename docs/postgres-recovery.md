# Postgres recovery runbook (structs-pg)

Use this when the container fails to start with checkpoint, WAL, or control-file errors—not as a routine restart procedure.

## 1. Capture evidence (before any reset)

```bash
docker logs structs-pg 2>&1 | tail -150
docker inspect structs-pg --format 'OOMKilled={{.State.OOMKilled}} ExitCode={{.State.ExitCode}}'
docker compose ps -a structs-pg-init
```

Inside the container (or via `docker exec`):

```bash
grep -E 'checkpoint|PANIC|FATAL|second postmaster|invalid|could not' \
  /var/log/postgresql/postgresql-*.log | tail -50
ls -la /var/lib/postgresql/*/main/pg_wal/
cat /var/lib/postgresql/*/main/postmaster.pid 2>/dev/null || true
```

| Log pattern | Likely cause |
|-------------|----------------|
| `second postmaster` / lock file | Two processes used the same data directory (init + main overlap, manual `database-init` while pg running) |
| `Killed` / host OOM | Reduce `shared_buffers` / raise container memory limit |
| `invalid checkpoint` / `could not find valid checkpoint` | Unclean shutdown (SIGKILL, stop timeout), OOM mid-checkpoint, or volume desync |
| `wrong timeline` / missing WAL file | Partial volume restore or copied data dir |

## 2. Ensure a single instance

```bash
docker compose stop structs-pg structs-pg-init structs-pg-auto-migrate
# Confirm nothing else mounts structs-pg-data
docker ps -a --filter volume=structs-pg-data
```

Do not run `database-init.sh` while `structs-pg` is up.

## 3. Try normal recovery first

Start only the main service and allow crash recovery to finish (can take minutes on large WAL):

```bash
docker compose up structs-pg
docker logs -f structs-pg
```

Wait until logs show `database system is ready to accept connections` or healthcheck passes.

## 4. When `pg_resetwal` is appropriate

`pg_resetwal` (PostgreSQL 18+) rewrites the control file and starts a new WAL timeline. **It can mask corruption.** Use only when:

- You have a **backup** of `pgdata` (and `pgetc` if still split).
- Only **one** instance has ever used this data directory since the failure.
- Normal startup fails after a clean single-instance stop, and logs point to an unrecoverable checkpoint/WAL state—not merely “shutting down” or “starting up”.

```bash
docker compose stop structs-pg
docker run --rm -it \
  -v structs-pg-data:/var/lib/postgresql \
  -v structs-pg-etc:/etc/postgresql \
  structs/structs-pg:latest bash

# inside container, as postgres:
su - postgres -c 'pg_resetwal -f /var/lib/postgresql/18/main'
# adjust path version if needed: ls /var/lib/postgresql/
```

Then start `structs-pg` and verify with `pg_isready` and application smoke tests. Re-index or restore from backup if you see data errors after reset.

## 5. Prevent recurrence

- Use the supervised `database-start.sh` CMD (no `bash` + `tail -f` wrapper, no `trap … exit 0`).
- Set `stop_grace_period: 120s` (or higher) on `structs-pg`.
- Match `POSTGRES_MEMORY_MB` and compose `deploy.resources.limits.memory`.
- Let `structs-pg-init` finish; avoid `compose down` during init migrations.
- Use `RUN_MIGRATIONS=1` only when you intend to run sqitch from init; ongoing schema changes belong in `structs-pg-auto-migrate`.

See also [postgres-volumes.md](postgres-volumes.md).
