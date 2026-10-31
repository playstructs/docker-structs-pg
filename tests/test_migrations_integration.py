#!/usr/bin/env python3
"""Test the packaged migrator and real Sqitch rollback in an isolated database.

Requires an already built image ID. Never connects to the running guild stack.
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import time
import uuid


BASELINE = 'role-structs-webapp-20261019-onboarding'
BOUNDARY = 'table-signer-tx-20261030-authority-contract'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True, help='Local immutable sha256 image ID')
    parser.add_argument('--scripts-dir', type=Path, help='Test local migration scripts instead of the image copies')
    args = parser.parse_args()
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', args.image):
        parser.error('--image must be an immutable local image ID')
    container = 'structs-v022-migrator-' + uuid.uuid4().hex[:12]

    def run(command, *, check=True, timeout=900):
        result = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
        print(result.stdout, end='', flush=True)
        print(result.stderr, end='', flush=True)
        if check and result.returncode:
            raise RuntimeError('Command failed: ' + ' '.join(command))
        return result

    def execute(*command, check=True):
        return run(['docker', 'exec', '-u', 'structs', '-e', 'PGHOST=/var/run/postgresql', '-e', 'PGOPTIONS=',
                    '-e', 'STRUCTS_PG_MIGRATION_MODE=pinned', '-e', 'STRUCTS_PG_MIGRATION_TARGET=' + BOUNDARY,
                    '-e', 'STRUCTS_LEDGER_WRITER_CONTRACT=', '-e', 'STRUCTS_SIGNER_CONTRACT=',
                    '-e', 'SQITCH_PG_CONNECTION=postgres://structs@/structs',
                    '-w', '/src/structs-pg', container, *command], check=check)

    def sql(statement):
        return execute('psql', '-XqAt', '-w', '-v', 'ON_ERROR_STOP=1', '-d', 'structs',
                       '-c', statement).stdout.strip()

    def latest():
        return sql("SELECT change FROM sqitch.changes WHERE project='structs-pg' ORDER BY committed_at DESC LIMIT 1;")

    ledger_query = "SELECT indexdef FROM pg_indexes WHERE schemaname='structs' AND indexname='ledger_source_event_uidx';"
    def start_database():
        mounts = []
        if args.scripts_dir:
            mounts = ['--mount', 'type=bind,src=' + str(args.scripts_dir.resolve()) + ',dst=/src/scripts,readonly']
        run(['docker', 'run', *mounts, '--pull=never', '-d', '--name', container,
             '--label', 'structs.task=v022-migrator-test', '--network', 'none',
             '--cpus', '2', '--memory', '4g', '--entrypoint', '/usr/bin/pg_ctlcluster', args.image,
             '--foreground', '-o', '-c shared_buffers=128MB -c max_connections=50 -c work_mem=4MB '
             '-c cron.launch_active_jobs=off -c timescaledb.max_background_workers=0',
             '18', 'main', 'start'])
        # Local peer authentication avoids copying any live credentials. Give
        # the test's OS user ownership of the bundled checkout for pinned Git.
        run(['docker', 'exec', container, 'chown', '-R', 'structs:structs', '/src/structs-pg'])
        for _ in range(60):
            if execute('pg_isready', '-d', 'structs', check=False).returncode == 0:
                break
            time.sleep(0.5)
        else:
            raise RuntimeError('Isolated database did not start')

    try:
        start_database()
        # Use the actual packaged schema, scripts and Sqitch configuration.
        execute('sqitch', 'deploy', '--verify', '--target', 'db:postgres://structs@/structs', '--to', BASELINE)
        assert latest() == BASELINE
        original_index = sql(ledger_query)
        assert original_index

        # An existing database must still fail at the gate and revert the
        # entire new batch. A mocked Sqitch cannot establish this behavior.
        failed = execute('env', 'STRUCTS_PG_MIGRATION_TARGET=latest',
                         '/src/scripts/database-migrate.sh', check=False)
        assert failed.returncode != 0
        assert 'paired ledger writer switch required' in failed.stderr
        assert latest() == BASELINE
        assert sql(ledger_query) == original_index

        execute('/src/scripts/database-migrate.sh')
        assert latest() == BOUNDARY
        assert sql(ledger_query) == original_index
        assert sql("SELECT count(*) FROM sqitch.changes WHERE change='table-ledger-20261029-denomination-identity';") == '0'
        events = sql('SELECT count(*) FROM sqitch.events;')
        execute('/src/scripts/database-migrate.sh')
        assert latest() == BOUNDARY
        assert sql('SELECT count(*) FROM sqitch.events;') == events
        assert sql(ledger_query) == original_index
        # Use a separate cluster for the fresh install: pg_cron connects to the
        # configured structs database even with scheduled jobs disabled.
        run(['docker', 'rm', '-f', container], timeout=60)
        start_database()
        for _ in range(2):
            execute('env', 'SQITCH_PG_CONNECTION=postgres://structs@/structs',
                    'STRUCTS_PG_MIGRATION_TARGET=latest', '/src/scripts/database-migrate.sh')
        fresh_tip = execute('psql', '-XqAt', '-w', '-d', 'structs', '-c',
                            "SELECT change FROM sqitch.changes ORDER BY committed_at DESC LIMIT 1;").stdout.strip()
        plan_tip = execute('sh', '-c', "awk '!/^($|%|#)/ { change=$1 } END { print change }' sqitch.plan").stdout.strip()
        assert fresh_tip == plan_tip, (fresh_tip, plan_tip)
        print(json.dumps({'result': 'pass', 'image': args.image,
                          'legacy_failure_reverts_to': BASELINE, 'bounded_deploy_stops_at': BOUNDARY,
                          'fresh_database_tip': fresh_tip, 'second_run': 'verification only', 'ledger_index_preserved': True}), flush=True)
    finally:
        # Only this test's unique, labelled container may be removed.
        inspect = subprocess.run(['docker', 'inspect', '--format',
                                  '{{index .Config.Labels "structs.task"}}', container],
                                 capture_output=True, text=True)
        if inspect.returncode == 0 and inspect.stdout.strip() == 'v022-migrator-test':
            run(['docker', 'rm', '-f', container], timeout=60)


if __name__ == '__main__':
    main()
