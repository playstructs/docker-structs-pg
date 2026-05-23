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

pg_cluster() {
  echo "main"
}

postgres_is_running() {
  local ver cluster status
  ver="$(pg_version_dir)"
  cluster="$(pg_cluster)"
  # pg_lsclusters output (with -h to suppress header):
  #   Ver Cluster Port Status Owner    Data directory                     Log file
  status="$(pg_lsclusters -h "${ver}" "${cluster}" 2>/dev/null | awk '{print $4}')"
  [[ "${status}" == "online" ]]
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
  local ver cluster
  ver="$(pg_version_dir)"
  cluster="$(pg_cluster)"
  if postgres_is_running; then
    echo "postgres already running (${ver}/${cluster})"
    postgres_wait_ready
    return 0
  fi
  echo "starting postgres (${ver}/${cluster})..."
  # pg_ctlcluster is Debian's wrapper around pg_ctl. It knows about the
  # split between /var/lib/postgresql (data) and /etc/postgresql (config)
  # and runs the server as the postgres user. Plain pg_ctl -D <datadir>
  # cannot find postgresql.conf on Debian without extra -o flags.
  # --skip-systemctl-redirect prevents an attempted handoff to systemctl
  # in containers where systemd is absent.
  pg_ctlcluster --skip-systemctl-redirect "${ver}" "${cluster}" start -- -w -t 120
  postgres_wait_ready
}

postgres_stop() {
  local mode="${1:-fast}"
  local timeout="${2:-115}"
  local ver cluster
  ver="$(pg_version_dir)"
  cluster="$(pg_cluster)"
  if ! postgres_is_running; then
    echo "postgres not running"
    return 0
  fi
  echo "stopping postgres (${ver}/${cluster}, mode=${mode}, timeout=${timeout}s)..."
  pg_ctlcluster --skip-systemctl-redirect "${ver}" "${cluster}" stop -- -m "${mode}" -w -t "${timeout}"
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
