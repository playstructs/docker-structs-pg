#!/usr/bin/env python3
"""Exercise influence migrations, permissions, projector and Go capture on a disposable PG18/Timescale image.

Uses minimal canonical entity tables, not the full game schema. No live DB access.
Requires Docker, Go, and a sibling structs-pg checkout (or --schema-dir).
"""
import argparse
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True, help='Already installed structs-pg image ID/tag')
    parser.add_argument('--grass-dir', type=Path, default=ROOT.parent / 'structs-grass')
    parser.add_argument('--schema-dir', type=Path, default=ROOT.parent / 'structs-pg')
    args = parser.parse_args()
    name = 'structs-influence-test-' + uuid.uuid4().hex[:10]

    def run(*cmd, **kwargs):
        return subprocess.run(cmd, text=True, check=True, **kwargs)

    def sql(source, database='postgres'):
        run('docker', 'exec', '-i', '-u', 'postgres', name, 'psql', '-Xq', '-h', '/tmp',
            '-U', 'postgres', '-d', database, '-v', 'ON_ERROR_STOP=1', input=source)

    def migration(operation, change):
        print(operation, change, flush=True)
        sql((args.schema_dir / operation / (change + '.sql')).read_text())

    image_id = subprocess.check_output(['docker', 'image', 'inspect', args.image, '--format', '{{.Id}}'], text=True).strip()
    arch = subprocess.check_output(['docker', 'image', 'inspect', image_id, '--format', '{{.Architecture}}'], text=True).strip()
    network = name + '-net'
    broker = name + '-nats'
    broker_files = tempfile.TemporaryDirectory(prefix='influence-nats-')
    broker_config = Path(broker_files.name) / 'nats.conf'
    broker_config.write_text('''authorization {
 users: [
  {user: relay, password: fixture, permissions: {publish: {allow: ["structs.influence.>"]}, subscribe: {deny: [">"]}}},
  {user: reader, password: fixture, permissions: {publish: {deny: [">"]}, subscribe: {allow: ["structs.influence.>"]}}}
 ]
}
''')
    try:
        run('docker', 'network', 'create', '--internal', network)
        run('docker', 'run', '--pull=never', '-d', '--name', broker, '--network', network, '--network-alias', 'broker', '--mount', f'type=bind,src={broker_config},dst=/etc/nats-test.conf,readonly', 'nats:latest', '-c', '/etc/nats-test.conf')
        run('docker', 'run', '--pull=never', '-d', '--name', name, '--network', network, '--user', 'postgres',
            '--tmpfs', '/tmp:rw,size=2147483648', '--entrypoint', '/bin/sh', image_id, '-ec',
            '/usr/lib/postgresql/18/bin/initdb -D /tmp/test-pg --auth=trust >/tmp/init.log; '
            'exec /usr/lib/postgresql/18/bin/postgres -D /tmp/test-pg -k /tmp '
            '-c listen_addresses= -c shared_preload_libraries=timescaledb -c shared_buffers=128MB '
            '-c max_parallel_workers_per_gather=0')
        for _ in range(120):
            ready = subprocess.run(['docker', 'exec', name, 'pg_isready', '-h', '/tmp', '-U', 'postgres'], capture_output=True)
            if ready.returncode == 0:
                break
            time.sleep(.25)
        else:
            raise RuntimeError('disposable database did not start')
        setup = '''CREATE EXTENSION timescaledb; CREATE SCHEMA structs;
CREATE ROLE structs_indexer; CREATE ROLE structs_webapp;
GRANT USAGE ON SCHEMA structs TO structs_indexer,structs_webapp;
CREATE TABLE structs.player(id text PRIMARY KEY,guild_id text);
CREATE TABLE structs.fleet(id text PRIMARY KEY,owner text);
CREATE TABLE structs.planet(id text PRIMARY KEY,owner text,sector smallint);
CREATE TABLE structs.current_block(chain text PRIMARY KEY,height bigint);
GRANT SELECT,UPDATE ON structs.player,structs.planet,structs.fleet TO structs_indexer;
GRANT SELECT ON structs.current_block TO structs_indexer;'''
        sql(setup)
        changes = ['influence-schema-20261111', 'influence-projector-20261111', 'influence-single-sector-20261111', 'influence-grass-20261111']
        for operation in ('deploy', 'verify', 'revert', 'deploy', 'verify'):
            for change in reversed(changes) if operation == 'revert' else changes:
                migration(operation, change)
        # Exercise the upgrade/backfill path against a populated old schema.
        sql('CREATE DATABASE influence_upgrade TEMPLATE template0;')
        sql(setup.replace('CREATE ROLE structs_indexer; CREATE ROLE structs_webapp;', ''), 'influence_upgrade')
        for change in changes[:-1]:
            sql((args.schema_dir/'deploy'/(change+'.sql')).read_text(), 'influence_upgrade')
        sql("""INSERT INTO structs.player VALUES('up-a','up-g1'),('up-d','up-g2');
SELECT structs.influence_begin_block('upgrade',1,'h1','2026-11-11T00:00:00Z',false);
SELECT structs.influence_create_contest('upgrade',1,3,1,'single_sector');
SELECT structs.influence_record_raid('{"chain":"upgrade","height":1,"phase":2,"tx_index":0,"event_index":1,"msg_index":0,"hash":"h1","time":"2026-11-11T00:00:00Z"}',
'{"attacker":"up-a","defender":"up-d","attacker_guild":"up-g1","defender_guild":"up-g2","planet":"up-p","sector":127}');
SELECT structs.influence_finish_block('upgrade',1,'h1');""", 'influence_upgrade')
        sql((args.schema_dir/'deploy/influence-grass-20261111.sql').read_text(), 'influence_upgrade')
        sql("""DO $$ BEGIN
IF (SELECT sum(unique_delta*100+claim_delta*200) FROM structs.influence_player_score_change WHERE player='up-a')<>300 THEN RAISE EXCEPTION 'backfill mismatch'; END IF;
IF NOT EXISTS(SELECT 1 FROM structs.influence_grass_frame WHERE payload->>'category'='influence_player' AND payload->'data'->>'points'='300') THEN RAISE EXCEPTION 'missing upgrade snapshot'; END IF;
END $$;""", 'influence_upgrade')
        sql('DROP DATABASE influence_upgrade;')
        with tempfile.TemporaryDirectory(prefix='influence-test-') as temp:
            binary = str(Path(temp) / 'influence-test')
            run('go', 'test', '-c', './internal/influence', '-o', binary, cwd=ROOT / 'sync-state',
                env={**os.environ, 'GOOS': 'linux', 'GOARCH': arch, 'CGO_ENABLED': '0'})
            run('docker', 'cp', binary, name + ':/usr/local/bin/influence-test')
            run('docker', 'exec', '-e', 'INFLUENCE_TEST_DATABASE_URL=host=/tmp user=postgres dbname=postgres sslmode=disable',
                '-e', 'INFLUENCE_TEST_MAINTENANCE=1', name, '/usr/local/bin/influence-test', '-test.v')
            grass_binary = str(Path(temp) / 'grass-test')
            run('go', 'test', '-c', '.', '-o', grass_binary, cwd=args.grass_dir,
                env={**os.environ, 'GOOS': 'linux', 'GOARCH': arch, 'CGO_ENABLED': '0'})
            run('docker', 'cp', grass_binary, name + ':/usr/local/bin/grass-test')
            run('docker', 'exec', '-e', 'GRASS_TEST_DATABASE_URL=host=/tmp user=postgres dbname=postgres sslmode=disable',
                '-e', 'GRASS_TEST_NATS_URL=nats://relay:fixture@broker:4222',
                '-e', 'GRASS_TEST_NATS_READER_URL=nats://reader:fixture@broker:4222', name, '/usr/local/bin/grass-test', '-test.v', '-test.timeout=60s')
        # A populated source cursor must make destructive rollback fail.
        sql("INSERT INTO structs.influence_source_cursor VALUES('revert-guard',1,0,'');")
        result = subprocess.run(['docker','exec','-i','-u','postgres',name,'psql','-Xq','-h','/tmp','-U','postgres','-d','postgres','-v','ON_ERROR_STOP=1'],
            input=(args.schema_dir/'revert'/'influence-projector-20261111.sql').read_text(),text=True,capture_output=True)
        assert result.returncode != 0 and 'populated' in result.stderr, result.stderr
        sql("DELETE FROM structs.influence_source_cursor WHERE chain_id='revert-guard';")
        result = subprocess.run(['docker','exec','-i','-u','postgres',name,'psql','-Xq','-h','/tmp','-U','postgres','-d','postgres','-v','ON_ERROR_STOP=1'],
            input=(args.schema_dir/'revert'/'influence-grass-20261111.sql').read_text(),text=True,capture_output=True)
        assert result.returncode != 0 and 'outbox is populated' in result.stderr, result.stderr
        sql('TRUNCATE structs.influence_grass_frame;')  # Disposable fixture only.
        for change in reversed(changes):
            migration('revert',change)
        print('PASS: migration lifecycle, protected revert, capture, scoring, transfers, rebuild, rewind, permissions and Timescale refresh')
    finally:
        subprocess.run(['docker', 'rm', '-f', name, broker], check=False)
        subprocess.run(['docker', 'network', 'rm', network], check=False)
        broker_files.cleanup()


if __name__ == '__main__':
    main()
