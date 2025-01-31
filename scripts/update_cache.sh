#!/usr/bin/env bash


echo "Updating Structs DB Cache based on chain data"

echo "Updating Allocation Data"
ALLOCATIONS_BLOB=`curl http://structsd:1317/structs/allocation`

ALLOCATION_COUNT=`echo ${ALLOCATIONS_BLOB} | jq ".Allocation" | jq length `

for (( p=0; p<ALLOCATION_COUNT; p++ ))
do
  ALLOCATION_BLOB=`echo ${ALLOCATIONS_BLOB} | jq ".Allocation[${p}]"`
  echo $ALLOCATION_BLOB > allocation.json

  psql -c "copy cache.tmp_json (data) from stdin" < allocation.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventAllocation.allocation',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Fleet Data"
FLEETS_BLOB=`curl http://structsd:1317/structs/fleet`

FLEET_COUNT=`echo ${FLEETS_BLOB} | jq ".Fleet" | jq length `

for (( p=0; p<FLEET_COUNT; p++ ))
do
  FLEET_BLOB=`echo ${FLEETS_BLOB} | jq ".Fleet[${p}]"`
  echo $FLEET_BLOB > fleet.json

  psql -c "copy cache.tmp_json (data) from stdin" < fleet.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventFleet.fleet',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Guild Data"
GUILDS_BLOB=`curl http://structsd:1317/structs/guild`

GUILD_COUNT=`echo ${GUILDS_BLOB} | jq ".Guild" | jq length `

for (( p=0; p<GUILD_COUNT; p++ ))
do
  GUILD_BLOB=`echo ${GUILDS_BLOB} | jq ".Guild[${p}]"`
  echo $GUILD_BLOB > guild.json

  psql -c "copy cache.tmp_json (data) from stdin" < guild.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventGuild.guild',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Infusion Data"
INFUSIONS_BLOB=`curl http://structsd:1317/structs/infusion`

INFUSION_COUNT=`echo ${INFUSIONS_BLOB} | jq ".Infusion" | jq length `

for (( p=0; p<INFUSION_COUNT; p++ ))
do
  INFUSION_BLOB=`echo ${INFUSIONS_BLOB} | jq ".Infusion[${p}]"`
  echo $INFUSION_BLOB > infusion.json

  psql -c "copy cache.tmp_json (data) from stdin" < infusion.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventInfusion.infusion',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"



echo "Updating Planet Data"
PLANETS_BLOB=`curl http://structsd:1317/structs/planet`

PLANET_COUNT=`echo ${PLANETS_BLOB} | jq ".Planet" | jq length `

for (( p=0; p<PLANET_COUNT; p++ ))
do
  PLANET_BLOB=`echo ${PLANETS_BLOB} | jq ".Planet[${p}]"`
  echo $PLANET_BLOB > planet.json

  psql -c "copy cache.tmp_json (data) from stdin" < planet.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventPlanet.planet',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Player Data"
PLAYERS_BLOB=`curl http://structsd:1317/structs/player`

PLAYER_COUNT=`echo ${PLAYERS_BLOB} | jq ".Player" | jq length `

for (( p=0; p<PLAYER_COUNT; p++ ))
do
  PLAYER_BLOB=`echo ${PLAYERS_BLOB} | jq ".Player[${p}]"`
  echo $PLAYER_BLOB > player.json

  psql -c "copy cache.tmp_json (data) from stdin" < player.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventPlayer.player',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Reactor Data"
REACTORS_BLOB=`curl http://structsd:1317/structs/reactor`

REACTOR_COUNT=`echo ${REACTORS_BLOB} | jq ".Reactor" | jq length `

for (( p=0; p<REACTOR_COUNT; p++ ))
do
  REACTOR_BLOB=`echo ${REACTORS_BLOB} | jq ".Reactor[${p}]"`
  echo $REACTOR_BLOB > reactor.json

  psql -c "copy cache.tmp_json (data) from stdin" < reactor.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventReactor.reactor',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Struct Type Data"
STRUCT_TYPES_BLOB=`curl http://structsd:1317/structs/struct_type`

STRUCT_TYPE_COUNT=`echo ${STRUCT_TYPES_BLOB} | jq ".StructType" | jq length `

for (( p=0; p<STRUCT_TYPE_COUNT; p++ ))
do
  STRUCT_TYPE_BLOB=`echo ${STRUCT_TYPES_BLOB} | jq ".StructType[${p}]"`
  echo $STRUCT_TYPE_BLOB > struct_type.json

  psql -c "copy cache.tmp_json (data) from stdin" < struct_type.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventStructType.structType',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"



echo "Updating Struct Data"
STRUCTS_BLOB=`curl http://structsd:1317/structs/struct`

STRUCT_COUNT=`echo ${STRUCTS_BLOB} | jq ".Struct" | jq length `

for (( p=0; p<STRUCT_COUNT; p++ ))
do
  STRUCT_BLOB=`echo ${STRUCTS_BLOB} | jq ".Struct[${p}]"`
  echo $STRUCT_BLOB > struct.json

  psql -c "copy cache.tmp_json (data) from stdin" < struct.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventStruct.structure',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Substation Data"
SUBSTATIONS_BLOB=`curl http://structsd:1317/structs/substation`

SUBSTATION_COUNT=`echo ${SUBSTATIONS_BLOB} | jq ".Substation" | jq length `

for (( p=0; p<SUBSTATION_COUNT; p++ ))
do
  SUBSTATION_BLOB=`echo ${SUBSTATIONS_BLOB} | jq ".Substation[${p}]"`
  echo $SUBSTATION_BLOB > substation.json

  psql -c "copy cache.tmp_json (data) from stdin" < substation.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventSubstation.substation',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Address Association Data"
ADDRESSES_BLOB=`curl http://structsd:1317/structs/address`

ADDRESS_COUNT=`echo ${ADDRESSES_BLOB} | jq ".address" | jq length `

for (( p=0; p<ADDRESS_COUNT; p++ ))
do
  ADDRESS_BLOB=`echo ${ADDRESSES_BLOB} | jq ".address[${p}]"`
  echo $ADDRESS_BLOB > address.json

  psql -c "copy cache.tmp_json (data) from stdin" < address.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventAddress.address',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"

echo "Updating Grid Data"
GRIDS_BLOB=`curl http://structsd:1317/structs/grid`

GRID_COUNT=`echo ${GRIDS_BLOB} | jq ".gridRecords" | jq length `

for (( p=0; p<GRID_COUNT; p++ ))
do
  GRID_BLOB=`echo ${GRIDS_BLOB} | jq ".gridRecords[${p}]"`
  echo $GRID_BLOB > grid.json

  psql -c "copy cache.tmp_json (data) from stdin" < grid.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventGrid.gridRecord',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Permission Data"
PERMISSIONS_BLOB=`curl http://structsd:1317/structs/permission`

PERMISSION_COUNT=`echo ${PERMISSIONS_BLOB} | jq ".permissionRecords" | jq length `

for (( p=0; p<PERMISSION_COUNT; p++ ))
do
  PERMISSION_BLOB=`echo ${PERMISSIONS_BLOB} | jq ".permissionRecords[${p}]"`
  echo $PERMISSION_BLOB > permission.json

  psql -c "copy cache.tmp_json (data) from stdin" < permission.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventPermission.permissionRecord',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"


echo "Updating Guild Membership Application Data"
GUILD_MEMBERSHIP_APPLICATIONS_BLOB=`curl http://structsd:1317/structs/guild_membership_application`

GUILD_MEMBERSHIP_APPLICATION_COUNT=`echo ${GUILD_MEMBERSHIP_APPLICATIONS_BLOB} | jq ".guildMembershipApplication" | jq length `

for (( p=0; p<GUILD_MEMBERSHIP_APPLICATION_COUNT; p++ ))
do
  GUILD_MEMBERSHIP_APPLICATION_BLOB=`echo ${GUILD_MEMBERSHIP_APPLICATIONS_BLOB} | jq ".guildMembershipApplication[${p}]"`
  echo GUILD_MEMBERSHIP_APPLICATION_BLOB > guild_membership_application.json

  psql -c "copy cache.tmp_json (data) from stdin" < guild_membership_application.json

done

psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT 'structs.structs.EventGuildMembershipApplication.guildMembershipApplication',tmp_json.data FROM cache.tmp_json"
psql -c "truncate cache.attributes_tmp"
psql -c "truncate cache.tmp_json"