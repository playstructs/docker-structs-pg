#!/usr/bin/env bash

echo "Updating Structs DB Cache based on chain data"
exec /usr/local/bin/update-cache "$@"
