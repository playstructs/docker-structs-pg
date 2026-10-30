#!/usr/bin/env bash

set -euo pipefail
# shellcheck source=migrations-common.sh
source "$(dirname "${BASH_SOURCE[0]}")/migrations-common.sh"

interval="${AUTO_MIGRATE_SLEEP:-120}"
if [[ ! "${interval}" =~ ^[1-9][0-9]*$ ]]; then
  echo "AUTO_MIGRATE_SLEEP must be a positive integer" >&2
  exit 1
fi
echo "Auto Migrator running every ${interval} seconds"

while true; do
  structs_migrate
  sleep "${interval}"
done
