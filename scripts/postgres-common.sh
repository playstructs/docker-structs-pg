#!/usr/bin/env bash
# Shared helpers for postgres init/start/stop (Debian/Ubuntu layout).

set -euo pipefail

pg_version_dir() {
  ls /etc/postgresql/ | sort -rn | head -1
}

pg_conf_dir() {
  echo "/etc/postgresql/$(pg_version_dir)/main"
}

pg_data_dir() {
  echo "/var/lib/postgresql/$(pg_version_dir)/main"
}

postgres_is_running() {
  local datadir
  datadir="$(pg_data_dir)"
  if [[ ! -f "${datadir}/postmaster.pid" ]]; then
    return 1
  fi
  su - postgres -c "pg_ctl status -D '${datadir}'" >/dev/null 2>&1
}

postgres_wait_ready() {
  local tries="${1:-60}"
  local i
  for ((i = 1; i <= tries; i++)); do
    if pg_isready -h localhost -p "${PGPORT:-5432}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "postgres did not become ready within ${tries}s" >&2
  return 1
}

postgres_start() {
  local datadir
  datadir="$(pg_data_dir)"
  if postgres_is_running; then
    echo "postgres already running (datadir=${datadir})"
    postgres_wait_ready
    return 0
  fi
  echo "starting postgres (datadir=${datadir})..."
  su - postgres -c "pg_ctl start -D '${datadir}' -w -t 120"
  postgres_wait_ready
}

postgres_stop() {
  local mode="${1:-fast}"
  local timeout="${2:-115}"
  local datadir
  datadir="$(pg_data_dir)"
  if ! postgres_is_running; then
    echo "postgres not running"
    return 0
  fi
  echo "stopping postgres (mode=${mode}, timeout=${timeout}s)..."
  su - postgres -c "pg_ctl stop -D '${datadir}' -m '${mode}' -w -t '${timeout}'"
}

postgres_postmaster_pid() {
  local pidfile
  pidfile="$(pg_data_dir)/postmaster.pid"
  if [[ ! -f "${pidfile}" ]]; then
    return 1
  fi
  head -1 "${pidfile}"
}

# Apply shared_buffers from POSTGRES_MEMORY_MB (~25% of limit) or POSTGRES_SHARED_BUFFERS.
postgres_apply_memory_settings() {
  local confdir dropin shared
  confdir="$(pg_conf_dir)"
  dropin="${confdir}/conf.d/structs-memory.conf"
  mkdir -p "${confdir}/conf.d"

  if [[ -n "${POSTGRES_SHARED_BUFFERS:-}" ]]; then
    shared="${POSTGRES_SHARED_BUFFERS}"
  elif [[ -n "${POSTGRES_MEMORY_MB:-}" ]]; then
    local mb quarter
    mb="${POSTGRES_MEMORY_MB}"
    quarter=$((mb / 4))
    if [[ "${quarter}" -lt 128 ]]; then
      quarter=128
    fi
    shared="${quarter}MB"
  else
    return 0
  fi

  cat >"${dropin}" <<EOF
# Generated at container start from POSTGRES_MEMORY_MB / POSTGRES_SHARED_BUFFERS.
shared_buffers = ${shared}
EOF
  chown postgres:postgres "${dropin}"
  echo "memory settings: shared_buffers=${shared} (${dropin})"
}
