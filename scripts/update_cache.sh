#!/usr/bin/env bash

# Helper function to fetch all paginated data for a given endpoint
fetch_all_paginated_data() {
    local endpoint=$1
    local data_key=$2
    local composite_key=$3
    local temp_file=$4
    
    echo "Fetching all $data_key data from $endpoint..."
    
    # Initialize variables
    local next_key=""
    local total_processed=0
    local page_count=0
    
    while true; do
        page_count=$((page_count + 1))
        
        # Build URL with pagination if needed
        local url="http://structsd:1317/structs/$endpoint"
        if [ ! -z "$next_key" ]; then
            url="$url?pagination.key=$next_key"
        fi
        
        echo "Fetching page $page_count from $url"
        
        # Fetch data
        local response=$(curl -s "$url")
        
        # Check if we got valid JSON
        if ! echo "$response" | jq . >/dev/null 2>&1; then
            echo "Error: Invalid JSON response from $url"
            break
        fi
        
        # Get the data array
        local data_array=$(echo "$response" | jq ".$data_key")
        
        # Check if data array exists and has items
        if [ "$data_array" = "null" ] || [ "$(echo "$data_array" | jq length)" -eq 0 ]; then
            echo "No more data found for $data_key"
            break
        fi
        
        # Process each item in the current page
        local item_count=$(echo "$data_array" | jq length)
        echo "Processing $item_count items from page $page_count"
        
        for (( i=0; i<item_count; i++ ))
        do
            local item=$(echo "$data_array" | jq ".[$i]")
            echo "$item" > "$temp_file"
            psql -c "copy cache.tmp_json (data) from stdin" < "$temp_file"
        done
        
        total_processed=$((total_processed + item_count))
        
        # Check for next page
        next_key=$(echo "$response" | jq -r '.pagination.next_key // empty')
        
        if [ -z "$next_key" ] || [ "$next_key" = "null" ]; then
            echo "No more pages for $data_key"
            break
        fi
        
        echo "Next page key: $next_key"
    done
    
    # Insert all collected data
    if [ $total_processed -gt 0 ]; then
        echo "Inserting $total_processed total $data_key records into cache"
        psql -c "INSERT INTO cache.attributes_tmp(composite_key, value) SELECT '$composite_key',tmp_json.data FROM cache.tmp_json"
        psql -c "truncate cache.attributes_tmp"
        psql -c "truncate cache.tmp_json"
    else
        echo "No $data_key data found"
    fi
    
    echo "Completed processing $data_key data"
}

echo "Updating Structs DB Cache based on chain data"

# Update Allocation Data
fetch_all_paginated_data "allocation" "Allocation" "structs.structs.EventAllocation.allocation" "allocation.json"

# Update Agreement Data
fetch_all_paginated_data "agreement" "Agreement" "structs.structs.EventAgreement.agreement" "agreement.json"

# Update Fleet Data
fetch_all_paginated_data "fleet" "Fleet" "structs.structs.EventFleet.fleet" "fleet.json"

# Update Guild Data
fetch_all_paginated_data "guild" "Guild" "structs.structs.EventGuild.guild" "guild.json"

# Update Infusion Data
fetch_all_paginated_data "infusion" "Infusion" "structs.structs.EventInfusion.infusion" "infusion.json"

# Update Planet Data
fetch_all_paginated_data "planet" "Planet" "structs.structs.EventPlanet.planet" "planet.json"

# Update Player Data
fetch_all_paginated_data "player" "Player" "structs.structs.EventPlayer.player" "player.json"

# Update Reactor Data
fetch_all_paginated_data "reactor" "Reactor" "structs.structs.EventReactor.reactor" "reactor.json"

# Update Provider Data
fetch_all_paginated_data "provider" "Provider" "structs.structs.EventProvider.provider" "provider.json"

# Update Struct Type Data
fetch_all_paginated_data "struct_type" "StructType" "structs.structs.EventStructType.structType" "struct_type.json"

# Update Struct Data
fetch_all_paginated_data "struct" "Struct" "structs.structs.EventStruct.structure" "struct.json"

# Update Substation Data
fetch_all_paginated_data "substation" "Substation" "structs.structs.EventSubstation.substation" "substation.json"

# Update Address Association Data
fetch_all_paginated_data "address" "address" "structs.structs.EventAddress.address" "address.json"

# Update Grid Data
fetch_all_paginated_data "grid" "gridRecords" "structs.structs.EventGrid.gridRecord" "grid.json"

# Update Permission Data
fetch_all_paginated_data "permission" "permissionRecords" "structs.structs.EventPermission.permissionRecord" "permission.json"

# Update Guild Membership Application Data
fetch_all_paginated_data "guild_membership_application" "guildMembershipApplication" "structs.structs.EventGuildMembershipApplication.guildMembershipApplication" "guild_membership_application.json"

echo "Cache update completed!"