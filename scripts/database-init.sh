#!/usr/bin/env bash

# launch the Structs database

# Variables
PORT=${PGPORT}

# SSL Config
if [ ! -f /src/structs/SSL_SETUP ]
then
  echo "Configuring SSL..."

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

  echo "hostssl    structs    structs    0.0.0.0/0    trust" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf
  echo "hostssl    structs    structs_indexer    0.0.0.0/0    trust" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf
  echo "hostssl    structs    structs_crawler    0.0.0.0/0    trust" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf
  echo "hostssl    structs    structs_webapp    0.0.0.0/0    trust" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf
  echo "hostssl    all    all    0.0.0.0/0    md5" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf

  touch /src/structs/SSL_SETUP
  echo "SSL Configured"
fi

echo "Checking database starts..."
/etc/init.d/postgresql start

echo "Pushing latest database schema..."
sed -i "s#SQITCH_PG_CONNECTION#${SQITCH_PG_CONNECTION}#" /src/structs/sqitch.conf
cd /src/structs
sqitch deploy

echo "Shutting down database..."
/etc/init.d/postgresql stop

echo "Initialization Done"

exit 0