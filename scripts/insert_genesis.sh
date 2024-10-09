#!/usr/bin/env bash

echo "Inserting Genesis Data into Structs DB"

echo "Deleting old stuff"
psql -c "DELETE FROM structs.ledger WHERE action = 'genesis';"

NETWORK_VERSION=$1
echo "Downloading current genesis (${NETWORK_VERSION}) ..."
git clone --depth 1 --branch $NETWORK_VERSION https://github.com/playstructs/structs-networks.git
ALL_GENESIS_BLOB=`cat structs-networks/genesis.json`

ALL_GENESIS_COUNT=`echo ${ALL_GENESIS_BLOB} | jq ".app_state.bank.balances" | jq length `
echo "Found ${ALL_GENESIS_COUNT} Genesis records"

ALL_GENESIS_TIME=`echo ${ALL_GENESIS_BLOB} | jq ".genesis_time" `

for (( p=0; p<ALL_GENESIS_COUNT; p++ ))
do
  GENESIS_ADDRESS=`echo ${ALL_GENESIS_BLOB} | jq ".app_state.bank.balances[${p}].address"`

  GENESIS_COUNT=`echo ${ALL_GENESIS_BLOB} | jq ".app_state.bank.balances[${p}].coins" | jq length `

  for (( c=0; c<GENESIS_COUNT; c++ ))
  do

    GENESIS_COIN_DENOM=`echo ${ALL_GENESIS_BLOB} | jq ".app_state.bank.balances[${p}].coins[${c}].denom"`
    GENESIS_COIN_AMOUNT=`echo ${ALL_GENESIS_BLOB} | jq ".app_state.bank.balances[${p}].coins[${c}].amount"`

    QUERY="INSERT INTO structs.ledger(address, amount, block_height, updated_at, created_at, action, direction, denom) VALUES('${GENESIS_ADDRESS}','${GENESIS_COIN_AMOUNT}', 0, NOW(), '${ALL_GENESIS_TIME}', 'genesis', 'credit', '${GENESIS_COIN_DENOM}'); "

    echo "${QUERY}"

    psql -c "${QUERY}"
  done
done
