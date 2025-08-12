#!/usr/bin/env bash

echo "Auto Migrator Running every ${AUTO_MIGRATE_SLEEP}"
cd /src/structs

while true; do
  git pull
  sqitch deploy db:pg:structs
  sleep ${AUTO_MIGRATE_SLEEP}
done
