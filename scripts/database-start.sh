#!/usr/bin/env bash

# launch the Structs database

# Variables
PORT=${PGPORT}

## Start database
/etc/init.d/postgresql start

## Watch log
tail -f /var/log/postgresql/postgresql-*.log
