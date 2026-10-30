#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=migrations-common.sh
source "$(dirname "${BASH_SOURCE[0]}")/migrations-common.sh"
structs_migrate
