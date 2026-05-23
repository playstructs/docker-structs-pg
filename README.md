# docker-structs-pg

[Docker](https://www.docker.com) container for running the [Structs Postgres Database](https://github.com/playstructs/structs-pg/). 

Docker Hub: [https://hub.docker.com/r/structs/structs-pg/](https://hub.docker.com/r/structs/structs-pg/)

## Structs
In the distant future the species of the galaxy are embroiled in a race for Alpha Matter, the rare and dangerous substance that fuels galactic civilization. Players take command of Structs, a race of sentient machines, and must forge alliances, conquer enemies and expand their influence to control Alpha Matter and the fate of the galaxy.

# How to Build

```
git clone git@github.com:playstructs/docker-structs-pg.git
cd docker-structs-pg
docker build .
```

# How to Use this Image

## Quickstart

The following will run the latest Structs postgres database server.

```
docker run -d --rm -p 5432:5432 --name=structs_pg structs/structs-pg:latest
```

## Interactive

A good way to run for development and for continual monitoring is to attach to the terminal:

```
docker run -it --rm -p 5432:5432 --name=structs_pg structs/structs-pg:latest
```

## Persistent volume

This image provides a persistent volume for `/var/lib/postgresql` if desired. If you wish to maintain the volume after the container is destroyed don't tell Docker to remove it with `--rm`. You can also override it:

```
docker run -d -p 5432:5432 -v /some/persistent/path:/var/lib/postgresql --name=structs_pg structs/structs-pg:latest
```

Stacks that also mount `/etc/postgresql` (config volume) must treat **data and config volumes as a pair**—see [docs/postgres-volumes.md](docs/postgres-volumes.md).

## Docker Compose

Use [docker-compose.example.yml](docker-compose.example.yml) for recommended `structs-pg` / `structs-pg-init` settings:

- Supervised entrypoint (`database-start.sh`) instead of a `bash` + `tail -f` wrapper
- `stop_grace_period: 120s` and memory limits aligned with `POSTGRES_MEMORY_MB`
- One-shot init (`SQITCH_INIT_COMPLETE` marker); set `RUN_MIGRATIONS=1` to force sqitch from init

## Operations

| Topic | Doc |
|--------|-----|
| Volume pairing and memory env | [docs/postgres-volumes.md](docs/postgres-volumes.md) |
| Checkpoint / WAL failures, `pg_resetwal` | [docs/postgres-recovery.md](docs/postgres-recovery.md) |

Environment variables (optional):

| Variable | Default | Purpose |
|----------|---------|---------|
| `POSTGRES_MEMORY_MB` | `8192` | Sets `shared_buffers` ≈ 25% via `conf.d/structs-memory.conf` |
| `POSTGRES_SHARED_BUFFERS` | — | Overrides computed `shared_buffers` (e.g. `2048MB`) |
| `POSTGRES_SHUTDOWN_MODE` | `fast` | `pg_ctl stop -m` mode on shutdown |
| `POSTGRES_SHUTDOWN_TIMEOUT` | `115` | Seconds to wait for stop before Docker SIGKILL |
| `RUN_MIGRATIONS` | `0` | Set to `1` on init container to force `sqitch deploy` |

# Administration

Administering the database must be done from within the container. After starting the container you can perform the following to attach to its terminal and access the database:

```
docker ps
[note container id of the structs container]
docker exec -it [container id] /bin/bash
su structs -c "psql structs"
```

# Configuration

## sqitch.conf

The primary configuration is handled in `conf/sqitch.conf`. Update this file prior to building to alter deployment.

# Learn more

- [Structs](https://playstructs.com)
- [Project Wiki](https://watt.wiki)
- [@PlayStructs Twitter](https://twitter.com/playstructs)


# License

Copyright 2021 [Slow Ninja Inc](https://slow.ninja).

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.