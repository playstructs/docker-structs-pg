#!/usr/bin/env bash
# One-shot init: SSL bootstrap, optional sqitch deploy, clean shutdown.
# Sqitch runs on first boot or when RUN_MIGRATIONS=1 (see postgres-common markers).

set -euo pipefail

# shellcheck source=postgres-common.sh
source /src/scripts/postgres-common.sh

PORT=${PGPORT:-5432}
INIT_MARKER="/etc/postgresql/SQITCH_INIT_COMPLETE"
SHUTDOWN_TIMEOUT="${POSTGRES_SHUTDOWN_TIMEOUT:-115}"

# SSL Config (once per pgetc volume)
if [[ ! -f /etc/postgresql/SSL_SETUP ]]; then
  echo "Configuring SSL..."

  export DOMAIN=structs-pg
  DATA_DIR="$(pg_conf_dir)"

  openssl genrsa -des3 -passout pass:x -out server.pass.key 2048
  openssl rsa -passin pass:x -in server.pass.key -out server.key
  rm server.pass.key
  openssl req -new -key server.key -out server.csr \
    -subj "/C=CC/ST=Ontarian/L=Torono/O=Struct/OU=Natural Resource Exploitation/CN=structs-pg"
  openssl x509 -req -days 365 -in server.csr -signkey server.key -out server.crt

  mv server.crt "${DATA_DIR}/server.crt"
  mv server.key "${DATA_DIR}/server.key"
  chown postgres:postgres "${DATA_DIR}/server.crt" "${DATA_DIR}/server.key"

  conf="$(pg_conf_dir)/postgresql.conf"
  hba="$(pg_conf_dir)/pg_hba.conf"
  echo "ssl = on" >>"${conf}"
  echo "ssl_cert_file = '${DATA_DIR}/server.crt'" >>"${conf}"
  echo "ssl_key_file = '${DATA_DIR}/server.key'" >>"${conf}"
  echo "ssl_prefer_server_ciphers = on" >>"${conf}"

  echo "hostssl    structs    structs    0.0.0.0/0    trust" >>"${hba}"
  echo "hostssl    structs    structs_indexer    0.0.0.0/0    trust" >>"${hba}"
  echo "hostssl    structs    structs_crawler    0.0.0.0/0    trust" >>"${hba}"
  echo "hostssl    structs    structs_webapp    0.0.0.0/0    trust" >>"${hba}"
  echo "hostssl    all    all    0.0.0.0/0    md5" >>"${hba}"

  touch /etc/postgresql/SSL_SETUP
  echo "SSL configured"
fi

run_sqitch=0
if [[ "${RUN_MIGRATIONS:-0}" == "1" ]]; then
  run_sqitch=1
elif [[ ! -f "${INIT_MARKER}" ]]; then
  run_sqitch=1
else
  echo "Skipping sqitch deploy (init already complete; set RUN_MIGRATIONS=1 to force)"
fi

postgres_apply_memory_settings

echo "Starting postgres for init..."
postgres_start

if [[ "${run_sqitch}" -eq 1 ]]; then
  echo "Pushing database schema (sqitch deploy)..."
  sed -i "s#SQITCH_PG_CONNECTION#${SQITCH_PG_CONNECTION}#" /src/structs-pg/sqitch.conf
  cd /src/structs-pg

  # Sentinel "origin" (compose default) means "use the branch the image was
  # cloned from at build time" - i.e. the upstream default. Any other value
  # is checked out from origin/<branch> so subsequent pulls track it.
  branch="${STRUCTS_PG_BRANCH:-origin}"
  git fetch --prune origin
  if [[ "${branch}" != "origin" ]]; then
    echo "Switching structs-pg to branch '${branch}'..."
    git checkout -B "${branch}" "origin/${branch}"
  fi
  current_branch="$(git rev-parse --abbrev-ref HEAD)"
  git pull --ff-only origin "${current_branch}"

  sqitch deploy
  touch "${INIT_MARKER}"
  echo "Sqitch deploy complete (branch=${current_branch} commit=$(git rev-parse --short HEAD)); marker written to ${INIT_MARKER}"
fi

echo "Shutting down postgres..."
postgres_stop fast "${SHUTDOWN_TIMEOUT}"

echo "Initialization done"
exit 0
