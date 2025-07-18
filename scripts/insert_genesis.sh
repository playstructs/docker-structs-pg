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

ALL_GENESIS_TIME=`echo ${ALL_GENESIS_BLOB} | jq -r ".genesis_time" `

for (( p=0; p<ALL_GENESIS_COUNT; p++ ))
do
  GENESIS_ADDRESS=`echo ${ALL_GENESIS_BLOB} | jq -r ".app_state.bank.balances[${p}].address"`

  GENESIS_COUNT=`echo ${ALL_GENESIS_BLOB} | jq -r ".app_state.bank.balances[${p}].coins" | jq length `

  for (( c=0; c<GENESIS_COUNT; c++ ))
  do

    GENESIS_COIN_DENOM=`echo ${ALL_GENESIS_BLOB} | jq -r ".app_state.bank.balances[${p}].coins[${c}].denom"`
    GENESIS_COIN_AMOUNT=`echo ${ALL_GENESIS_BLOB} | jq -r ".app_state.bank.balances[${p}].coins[${c}].amount"`

    QUERY="INSERT INTO structs.ledger(address, amount_p, block_height, time, action, direction, denom) VALUES('${GENESIS_ADDRESS}','${GENESIS_COIN_AMOUNT}', 0, '${ALL_GENESIS_TIME}', 'genesis', 'credit', '${GENESIS_COIN_DENOM}'); "

    echo "${QUERY}"

    psql -c "${QUERY}"
  done
done


# Yeah, it's stupid, but it'll work for now. TODO Fix to be generic or fix on new genesis
QUERY="INSERT INTO structs.ledger(address, counterparty, amount_p, block_height, time, action, direction, denom) VALUES('structs1ul8sd7nk573aw2gyzzwn2ahxqzrq0qg70en5e9','structsvaloper1ul8sd7nk573aw2gyzzwn2ahxqzrq0qg7f8g7t2','300000000', 0, '${ALL_GENESIS_TIME}', 'genesis', 'debit', 'ualpha'); "
echo "${QUERY}"
psql -c "${QUERY}"
QUERY="INSERT INTO structs.ledger(address, counterparty, amount_p, block_height, time, action, direction, denom) VALUES('structs1ul8sd7nk573aw2gyzzwn2ahxqzrq0qg70en5e9','structsvaloper1ul8sd7nk573aw2gyzzwn2ahxqzrq0qg7f8g7t2','300000000', 0, '${ALL_GENESIS_TIME}', 'genesis', 'credit', 'ualpha.infused'); "
echo "${QUERY}"
psql -c "${QUERY}"
QUERY="INSERT INTO structs.ledger(counterparty, address, amount_p, block_height, time, action, direction, denom) VALUES('structs1ul8sd7nk573aw2gyzzwn2ahxqzrq0qg70en5e9','structsvaloper1ul8sd7nk573aw2gyzzwn2ahxqzrq0qg7f8g7t2','300000000', 0, '${ALL_GENESIS_TIME}', 'genesis', 'credit', 'ualpha.infused'); "
echo "${QUERY}"
psql -c "${QUERY}"