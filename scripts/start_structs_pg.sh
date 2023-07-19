#!/usr/bin/env bash

# launch the Structs database

# Variables
PORT=5432

# Logic

## Start database
/etc/init.d/postgresql start

## Start tic.pl
#su structs -c '/src/structs/monitor.pl | tee /src/structs/monitor.log 2>&1 &'

## Watch log
tail -f /var/log/postgresql/postgresql-*.log
