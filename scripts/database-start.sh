#!/usr/bin/env bash
# Supervised Postgres entrypoint for structs-pg (Docker CMD).
# Forwards SIGTERM to pg_ctl stop; exits non-zero if stop or postgres fails.

set -euo pipefail

# shellcheck source=postgres-common.sh
source /src/scripts/postgres-common.sh

SHUTDOWN_MODE="${POSTGRES_SHUTDOWN_MODE:-fast}"
SHUTDOWN_TIMEOUT="${POSTGRES_SHUTDOWN_TIMEOUT:-115}"

stop_requested=0
exit_code=0

on_signal() {
  stop_requested=1
}

trap on_signal SIGTERM SIGINT SIGQUIT

if ! postgres_acquire_datadir_lock; then
  echo "FATAL: another postmaster owns ${POSTGRES_DATADIR_LOCK_FILE}; refusing to start to avoid corruption" >&2
  exit 1
fi

postgres_apply_memory_settings
postgres_start

pid="$(postgres_postmaster_pid)"
echo "supervising postgres pid=${pid}"

while [[ "${stop_requested}" -eq 0 ]]; do
  if ! kill -0 "${pid}" 2>/dev/null; then
    echo "postgres exited unexpectedly" >&2
    exit_code=1
    break
  fi
  sleep 2
done

if postgres_is_running; then
  postgres_stop "${SHUTDOWN_MODE}" "${SHUTDOWN_TIMEOUT}" || exit_code=1
fi

exit "${exit_code}"
