#!/usr/bin/env bash

echo "Auto Migrator Running every ${AUTO_MIGRATE_SLEEP}"
cd /src/structs

while true; do
  git pull
  su - structs -c 'cd /src/structs && sqitch deploy db:pg:structs'
  sleep ${AUTO_MIGRATE_SLEEP}
done
