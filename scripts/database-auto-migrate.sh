#!/usr/bin/env bash

echo "Auto Migrator Running every ${AUTO_MIGRATE_SLEEP}"
cd /src/structs-pg

sed -i "s#SQITCH_PG_CONNECTION#${SQITCH_PG_CONNECTION}#" /src/structs-pg/sqitch.conf

# Sentinel "origin" (compose default) means "use the branch the image was
# cloned from at build time". Any other value triggers a one-time checkout
# against origin/<branch> so subsequent pulls in the loop track it.
branch="${STRUCTS_PG_BRANCH:-origin}"
git fetch --prune origin || true
if [[ "${branch}" != "origin" ]]; then
  echo "Switching structs-pg to branch '${branch}'..."
  git checkout -B "${branch}" "origin/${branch}"
fi
echo "Auto-migrate tracking structs-pg branch: $(git rev-parse --abbrev-ref HEAD)"

while true; do
  git pull
  sqitch deploy
  sleep ${AUTO_MIGRATE_SLEEP}
done
