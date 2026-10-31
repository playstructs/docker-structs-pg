#!/usr/bin/env bash
# Shared by one-shot initialization and the recurring migrator.

structs_migration_checkout() {
  cd "${STRUCTS_PG_DIR:-/src/structs-pg}" || return
  # sqitch.conf is an image configuration overlay; migration sources must
  # remain exactly those supplied by the selected revision.
  git diff --quiet -- . ':(exclude)sqitch.conf' && git diff --cached --quiet || {
    echo "Refusing migrations from modified tracked sources" >&2
    return 1
  }
  local mode="${STRUCTS_PG_MIGRATION_MODE:-main}"
  local branch="${STRUCTS_PG_BRANCH:-origin}"
  local ref="${STRUCTS_PG_REF:-}"
  case "${mode}" in
    pinned)
      if [[ ! "${ref}" =~ ^[0-9a-f]{40}$ ]]; then
        echo "Pinned migrations require a full STRUCTS_PG_REF commit SHA" >&2
        return 1
      fi
      if [[ "${branch}" != "origin" && -n "${branch}" ]]; then
        echo "STRUCTS_PG_BRANCH requires explicit STRUCTS_PG_MIGRATION_MODE=branch" >&2
        return 1
      fi
      if ! git cat-file -e "${ref}^{commit}" 2>/dev/null; then
        git fetch --no-tags --depth=1 origin "${ref}" || return
      fi
      git checkout --detach "${ref}" || return
      [[ "$(git rev-parse HEAD)" == "${ref}" ]] || return 1
      ;;
    main|branch)
      # Default automation follows main. Keep the old Compose sentinel working;
      # an explicit branch override still selects that branch.
      if [[ "${mode}" == "main" && "${branch}" == "origin" ]]; then
        branch=main
      fi
      if [[ -z "${branch}" || "${branch}" == "origin" ]] || ! git check-ref-format "refs/heads/${branch}"; then
        echo "Branch migration mode requires an explicit STRUCTS_PG_BRANCH name" >&2
        return 1
      fi
      git fetch --no-tags origin "refs/heads/${branch}" || return
      git checkout --detach FETCH_HEAD || return
      ;;
    *)
      echo "Unknown STRUCTS_PG_MIGRATION_MODE: ${mode}" >&2
      return 1
      ;;
  esac
  echo "Migration source: mode=${mode} commit=$(git rev-parse HEAD)"
}

structs_migrate() {
  local target="${STRUCTS_PG_MIGRATION_TARGET:-latest}"
  local ledger="${STRUCTS_LEDGER_WRITER_CONTRACT:-}"
  local signer="${STRUCTS_SIGNER_CONTRACT:-}"
  if [[ ! "${target}" =~ ^[a-zA-Z0-9][a-zA-Z0-9_/-]*$ ]]; then
    echo "STRUCTS_PG_MIGRATION_TARGET must name a Sqitch change or latest" >&2
    return 1
  fi
  if [[ -n "${ledger}" && "${ledger}" != "denomination-v1" ]] || [[ -n "${signer}" && "${signer}" != "v022-1" ]]; then
    echo "Unrecognized writer or signer migration contract" >&2
    return 1
  fi
  if [[ "${SQITCH_PG_CONNECTION:-}" != postgres://* && "${SQITCH_PG_CONNECTION:-}" != postgresql://* ]]; then
    echo "SQITCH_PG_CONNECTION must be a PostgreSQL URI" >&2
    return 1
  fi
  structs_migration_checkout || return
  # A genuinely empty database has no old writer/signer state to transition.
  # Inspect the database, not a volume marker: a lost marker must never opt an
  # existing deployment into an incompatible migration.
  if [[ -z "${ledger}" || -z "${signer}" ]]; then
    local fresh
    fresh=$(psql "${SQITCH_PG_CONNECTION}" -XqAt -v ON_ERROR_STOP=1 <<'SQL'
-- migration_fresh_database
SELECT to_regclass('sqitch.changes') IS NOT NULL AS registry_exists \gset
SELECT NOT EXISTS (
  SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='structs'
) AS application_empty \gset
\if :registry_exists
SELECT :'application_empty'::boolean AND NOT EXISTS (SELECT 1 FROM sqitch.changes);
\else
SELECT :'application_empty'::boolean;
\endif
SQL
    ) || return
    case "${fresh}" in
      t)
        ledger="${ledger:-denomination-v1}"
        signer="${signer:-v022-1}"
        ;;
      f) ;;
      *) echo "Unexpected migration initialization state" >&2; return 1 ;;
    esac
  fi
  local options="${PGOPTIONS:-}"
  if [[ -n "${ledger}" ]]; then
    options+=" -c structs.ledger_writer_contract=${ledger}"
  fi
  if [[ -n "${signer}" ]]; then
    options+=" -c structs.signer_contract=${signer}"
  fi
  local args=(deploy --verify --target "db:${SQITCH_PG_CONNECTION}")
  if [[ "${target}" != "latest" ]]; then
    args+=(--to "${target}")
    # A database may already be beyond this safe automation boundary after
    # an operator's coordinated switch. Sqitch refuses deploy --to an older
    # change. Read its registry, then verify that boundary without reverting
    # or treating an arbitrary deployment error as success. Do not rely on
    # verify alone: Sqitch also returns success for an empty database.
    local deployed
    deployed=$(psql "${SQITCH_PG_CONNECTION}" -XqAt -v ON_ERROR_STOP=1 -v change="${target}" <<'SQL'
SELECT to_regclass('sqitch.changes') IS NOT NULL AS registry_exists \gset
\if :registry_exists
SELECT EXISTS(SELECT 1 FROM sqitch.changes WHERE project='structs-pg' AND change=:'change');
\else
SELECT FALSE;
\endif
SQL
    ) || return
    case "${deployed}" in
      t)
        PGOPTIONS="${options}" sqitch verify --target "db:${SQITCH_PG_CONNECTION}" --from "${target}" --to "${target}"
        return
        ;;
      f) ;;
      *) echo "Unexpected Sqitch registry state" >&2; return 1 ;;
    esac
  fi
  echo "Deploying Sqitch target: ${target}"
  PGOPTIONS="${options}" sqitch "${args[@]}"
}
