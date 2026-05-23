# Build update-cache Go binary
FROM golang:1.25 AS update-cache-builder
WORKDIR /build
COPY update-cache/ .
RUN go mod download && \
    CGO_ENABLED=0 go build -o /update-cache ./cmd/

# Build sync-state Go binary
FROM golang:1.25 AS sync-state-builder
WORKDIR /build
COPY sync-state/ .
RUN go mod download && \
    CGO_ENABLED=0 go build -o /sync-state ./cmd/sync-state

# Base image
FROM ubuntu:24.04

# Information
LABEL maintainer="Slow Ninja <info@slow.ninja>"

# Variables
ENV DEBIAN_FRONTEND=noninteractive \
  PGDATABASE=structs \
  PGPORT=5432 \
  PGHOST=localhost \
  PGUSER=structs \
  SSL_DOMAIN=structs.lol \
  NETWORK_VERSION=main \
  AUTO_MIGRATE_SLEEP=120 \
  SQITCH_PG_CONNECTION=postgres://structs@structs-pg:5432/structs \
  POSTGRES_MEMORY_MB=8192 \
  POSTGRES_SHUTDOWN_MODE=fast \
  POSTGRES_SHUTDOWN_TIMEOUT=115



# Install packages
RUN apt-get update && \
    apt-get upgrade -y && \
    apt-get install -y \
        build-essential \
        curl \
        jq \
        git \
        perl \
        postgresql-common \
        lsb-release \
        apt-transport-https \
        openssl \
        wget \
        gnupg \
        vim

RUN echo "deb https://packagecloud.io/timescale/timescaledb/ubuntu/ $(lsb_release -c -s) main" | tee /etc/apt/sources.list.d/timescaledb.list
RUN wget --quiet -O - https://packagecloud.io/timescale/timescaledb/gpgkey | gpg --dearmor -o /etc/apt/trusted.gpg.d/timescaledb.gpg


RUN  sed -i "s/read enter//g" /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh
RUN  cat /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh && \
     /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh && \
     apt-get -y install \
            postgresql-18-cron \
            timescaledb-2-postgresql-18 \
            postgresql \
            postgresql-client \
            postgresql-server-dev-all \
     	    sqitch \
     	    libdbd-pg-perl \
     	    libdbd-sqlite3-perl \
     	    sqlite3


RUN rm -rf /var/lib/apt/lists/*

# Add the user and groups appropriately
RUN addgroup --system structs && \
    adduser --system --home /src/structs --shell /bin/bash --group structs

# Clone down structs-pg for database schematics
WORKDIR /src
RUN mkdir /src/scripts

RUN git clone https://github.com/playstructs/structs-pg.git structs-pg
COPY conf/sqitch.conf /src/structs-pg/
COPY scripts/ /src/scripts/
RUN chmod +x /src/scripts/*.sh

# Copy pre-built Go binaries from builder stages
COPY --from=update-cache-builder /update-cache /usr/local/bin/update-cache
COPY --from=sync-state-builder   /sync-state   /usr/local/bin/sync-state

# Deploy Structs PG
RUN sed -i "s/^#listen_addresses.*\=.*'localhost/listen_addresses = '\*/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    sed -i "s/^#shared_preload_libraries.*/shared_preload_libraries = 'timescaledb,pg_cron'/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    echo "cron.database_name = 'structs'" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    echo "cron.use_background_workers = on" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    echo "max_worker_processes = 20" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    mkdir -p /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/conf.d && \
    sed -i "/^host.*all.*all.*127\.0\.0\.1\/32.*md5$/s/md5/trust/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    sed -i "/^host.*all.*all.*::1\/128.*md5$/s/md5/trust/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    #echo "host structs +players ::/0 md5" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    #echo "host structs +players 0.0.0.0/0 md5" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    /etc/init.d/postgresql start && \
    su - postgres -c 'createuser -s structs && createdb -O structs structs' && \
    su - postgres -c 'createuser -s structs_indexer' && \
    su - postgres -c 'createuser -s structs_crawler' && \
    su - postgres -c 'createuser -s structs_webapp' && \
    timescaledb-tune --quiet --yes && \
    /etc/init.d/postgresql stop

# Expose ports
EXPOSE 5432

# Persistence volume
# VOLUME [ "/var/lib/postgresql" ]

# Run Structs
CMD [ "/src/scripts/database-start.sh" ]
