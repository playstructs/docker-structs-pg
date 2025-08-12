#!/usr/bin/env bash

echo "Auto Migrator Running every ${AUTO_MIGRATE_SLEEP}"
cd /src/structs-pg

sed -i "s#SQITCH_PG_CONNECTION#${SQITCH_PG_CONNECTION}#" /src/structs/sqitch.conf

while true; do
  git pull
  sqitch deploy
  sleep ${AUTO_MIGRATE_SLEEP}
done
