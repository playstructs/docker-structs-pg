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
  NETWORK_VERSION=main



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
        gnupg

RUN echo "deb https://packagecloud.io/timescale/timescaledb/ubuntu/ $(lsb_release -c -s) main" | tee /etc/apt/sources.list.d/timescaledb.list
RUN wget --quiet -O - https://packagecloud.io/timescale/timescaledb/gpgkey | gpg --dearmor -o /etc/apt/trusted.gpg.d/timescaledb.gpg


RUN  sed -i "s/read enter//g" /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh
RUN  cat /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh && \
     /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh && \
     apt-get -y install \
            postgresql-17-cron \
            timescaledb-2-postgresql-17 \
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
RUN git clone https://github.com/playstructs/structs-pg.git structs
RUN chown -R structs /src/structs 
COPY conf/sqitch.conf /src/structs/
COPY scripts/* /src/structs/

# Deploy Structs PG
RUN sed -i "s/^#listen_addresses.*\=.*'localhost/listen_addresses = '\*/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    sed -i "s/^#shared_preload_libraries.*/shared_preload_libraries = 'timescaledb,pg_cron'/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    echo "cron.database_name = 'structs'" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    echo "cron.use_background_workers = on" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    echo "shared_buffers = 3072MB" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    echo "max_worker_processes = 20" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/postgresql.conf && \
    sed -i "/^host.*all.*all.*127\.0\.0\.1\/32.*md5$/s/md5/trust/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    sed -i "/^host.*all.*all.*::1\/128.*md5$/s/md5/trust/g" /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    #echo "host structs +players ::/0 md5" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    #echo "host structs +players 0.0.0.0/0 md5" >> /etc/postgresql/$(ls /etc/postgresql/ | sort -r |head -1)/main/pg_hba.conf && \
    /etc/init.d/postgresql start && \
    su - postgres -c 'createuser -s structs && createdb -O structs structs' && \
    su - postgres -c 'createuser -s structs_indexer' && \
    su - postgres -c 'createuser -s structs_webapp' && \
    su - structs -c 'cd /src/structs && sqitch deploy db:pg:structs' && \
    timescaledb-tune --quiet --yes \
    /etc/init.d/postgresql stop

# Expose ports
EXPOSE 5432

# Persistence volume
# VOLUME [ "/var/lib/postgresql" ]

# Run Structs
CMD [ "/src/structs/start_structs_pg.sh" ]
