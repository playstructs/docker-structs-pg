#!/usr/bin/env bash

# launch the Structs database

# Variables
PORT=5432

# Logic
if [ ! -f /src/structs/SSL_SETUP ]
then
  export DOMAIN=structs-pg
  export DATA_DIR=/etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main

  openssl genrsa -des3 -passout pass:x -out server.pass.key 2048
  openssl rsa -passin pass:x -in server.pass.key -out server.key
  rm server.pass.key
  openssl req -new -key server.key -out server.csr \
          -subj "/C=CC/ST=Ontarian/L=Torono/O=Struct/OU=Natural Resource Exploitation/CN=structs-pg" && \

  openssl x509 -req -days 365 -in server.csr -signkey server.key -out server.crt

  mv server.crt $DATA_DIR/server.crt
  mv server.key $DATA_DIR/server.key

  chown postgres:postgres  $DATA_DIR/server.crt $DATA_DIR/server.key

  echo "ssl = on" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf
  echo "ssl_cert_file = '/etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/server.crt'" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf
  echo "ssl_key_file = '/etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/server.key'" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf
  echo "ssl_prefer_server_ciphers = on" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf

  echo "hostssl    structs    structs_indexer    0.0.0.0/0    trust" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf
  echo "hostssl    structs    structs_webapp    0.0.0.0/0    trust" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf
  echo "hostssl    all    all    0.0.0.0/0    md5" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf

  touch /src/structs/SSL_SETUP
fi

## Start database
/etc/init.d/postgresql start

echo "Inserting Genesis Data..."
su - structs -c 'bash /src/structs/insert_genesis.sh'
echo "Genesis Data inserted..."

# Wait for the node to be alive
echo "Waiting for structsd Node"

NODE_LIVENESS="true"
while [[ $NODE_LIVENESS == "true" ]]
do
  NODE_LIVENESS=`curl http://structsd:26657/status -s -f  | jq -r .result.sync_info.catching_up`
done

su - structs -c 'bash /src/structs/update_cache.sh'

if [[ -n "${GUILD_ID// /}" ]]; then
  echo "insert into structs.guild_meta(id, name, description, tag, logo, socials, website, this_infrastructure, created_at, updated_at) VALUES( '$GUILD_ID','$GUILD_NAME','$GUILD_DESCRIPTION','$GUILD_TAG','$GUILD_LOGO','$GUILD_SOCIALS','$GUILD_WEBSITE','t',NOW(),NOW()) ON CONFLICT (id) DO UPDATE SET id = EXCLUDED.id, name = EXCLUDED.name, description = EXCLUDED.description, tag = EXCLUDED.tag, logo = EXCLUDED.logo, socials = EXCLUDED.socials, website = EXCLUDED.website, this_infrastructure='t', updated_at = NOW()" >> /src/structs/guild.sql
  su - structs -c "psql -f /src/structs/guild.sql"
fi

## Watch log
tail -f /var/log/postgresql/postgresql-*.log
