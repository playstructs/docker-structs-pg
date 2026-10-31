#!/usr/bin/env python3
"""Exercise migration selection against real Git repositories; never connects to a DB."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
BOUNDARY = 'table-signer-tx-20261030-authority-contract'


class MigrationScripts(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='structs-migrations-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.origin = self.root / 'origin'
        self.checkout = self.root / 'checkout'
        self.log = self.root / 'calls.jsonl'
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.git('init', '-b', 'main', str(self.origin))
        self.git('-C', str(self.origin), 'config', 'user.name', 'Migration test')
        self.git('-C', str(self.origin), 'config', 'user.email', 'test@invalid')
        (self.origin / 'migration.sql').write_text('first\n')
        self.git('-C', str(self.origin), 'add', '.')
        self.git('-C', str(self.origin), 'commit', '-m', 'first')
        self.first = self.git('-C', str(self.origin), 'rev-parse', 'HEAD')
        self.git('clone', str(self.origin), str(self.checkout))
        (self.origin / 'migration.sql').write_text('second\n')
        self.git('-C', str(self.origin), 'commit', '-am', 'second')
        self.second = self.git('-C', str(self.origin), 'rev-parse', 'HEAD')
        (self.bin / 'sqitch').write_text(f'#!{sys.executable}\n' + '''import json, os, sys
with open(os.environ['CALL_LOG'],'a') as f:
    f.write(json.dumps({'args':sys.argv[1:],'options':os.environ.get('PGOPTIONS')})+'\\n')
sys.exit(int(os.environ.get('SQITCH_EXIT','0')))
''')
        (self.bin / 'psql').write_text(f'#!{sys.executable}\n' + '''import os, sys
query = sys.stdin.read()
setting = 'REGISTRY_EMPTY' if 'migration_fresh_database' in query else 'REGISTRY_DEPLOYED'
print(os.environ.get(setting,'f'))
sys.exit(int(os.environ.get('PSQL_EXIT','0')))
''')
        for file in self.bin.iterdir():
            file.chmod(0o755)
        self.env = {**os.environ, 'PATH': str(self.bin) + os.pathsep + os.environ['PATH'],
                    'CALL_LOG': str(self.log), 'STRUCTS_PG_DIR': str(self.checkout),
                    'STRUCTS_PG_REF': self.first, 'STRUCTS_PG_MIGRATION_MODE': 'pinned',
                    'STRUCTS_PG_BRANCH': 'origin', 'STRUCTS_PG_MIGRATION_TARGET': BOUNDARY,
                    'SQITCH_PG_CONNECTION': 'postgres://test@localhost/test',
                    'STRUCTS_LEDGER_WRITER_CONTRACT': '', 'STRUCTS_SIGNER_CONTRACT': '', 'PGOPTIONS': ''}

    def git(self, *args):
        return subprocess.check_output(['git', *args], text=True, stderr=subprocess.DEVNULL).strip()

    def run_script(self, script='database-migrate.sh', **env):
        environment = {**self.env, **env}
        environment = {key: value for key, value in environment.items() if value is not None}
        return subprocess.run(['bash', str(ROOT / 'scripts' / script)], env=environment,
                              capture_output=True, text=True, timeout=8)

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def test_pin_ignores_moving_main_and_works_without_origin(self):
        self.origin.rename(self.root / 'unavailable')
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git('-C', str(self.checkout), 'rev-parse', 'HEAD'), self.first)
        self.assertEqual(self.calls()[0]['args'], ['deploy', '--verify', '--target',
                         'db:postgres://test@localhost/test', '--to', BOUNDARY])
        self.assertEqual(self.calls()[0]['options'], '')

    def test_explicit_branch_mode_tracks_branch_but_retains_target(self):
        result = self.run_script(STRUCTS_PG_MIGRATION_MODE='branch', STRUCTS_PG_BRANCH='main')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git('-C', str(self.checkout), 'rev-parse', 'HEAD'), self.second)
        self.assertEqual(self.calls()[0]['args'][-2:], ['--to', BOUNDARY])

    def test_default_and_main_mode_follow_remote_main(self):
        for mode in [None, '', 'main']:
            for branch in [None, '', 'origin', 'main']:
                with self.subTest(mode=mode, branch=branch):
                    self.git('-C', str(self.checkout), 'checkout', '--detach', self.first)
                    result = self.run_script(STRUCTS_PG_MIGRATION_MODE=mode,
                                             STRUCTS_PG_BRANCH=branch)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.git('-C', str(self.checkout), 'rev-parse', 'HEAD'), self.second)
                    self.assertEqual(self.calls()[-1]['args'][-2:], ['--to', BOUNDARY])

    def test_main_mode_preserves_explicit_branch_override(self):
        self.git('-C', str(self.origin), 'branch', 'release/test', self.first)
        result = self.run_script(STRUCTS_PG_MIGRATION_MODE='main', STRUCTS_PG_BRANCH='release/test')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git('-C', str(self.checkout), 'rev-parse', 'HEAD'), self.first)

    def test_main_mode_fetches_new_commits_on_each_run(self):
        self.assertEqual(self.run_script(STRUCTS_PG_MIGRATION_MODE='main').returncode, 0)
        (self.origin / 'migration.sql').write_text('third\n')
        self.git('-C', str(self.origin), 'commit', '-am', 'third')
        third = self.git('-C', str(self.origin), 'rev-parse', 'HEAD')
        result = self.run_script(STRUCTS_PG_MIGRATION_MODE='main')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git('-C', str(self.checkout), 'rev-parse', 'HEAD'), third)

    def test_main_mode_fetch_failure_does_not_deploy_stale_checkout(self):
        self.origin.rename(self.root / 'unavailable')
        result = self.run_script(STRUCTS_PG_MIGRATION_MODE='main')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.calls(), [])

    def test_default_target_deploys_latest_on_fresh_database(self):
        result = self.run_script(STRUCTS_PG_MIGRATION_MODE=None,
                                 STRUCTS_PG_MIGRATION_TARGET=None,
                                 STRUCTS_PG_BRANCH='main', REGISTRY_EMPTY='t')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git('-C', str(self.checkout), 'rev-parse', 'HEAD'), self.second)
        self.assertEqual(self.calls()[0]['args'], ['deploy', '--verify', '--target',
                                                 'db:postgres://test@localhost/test'])
        self.assertEqual(self.calls()[0]['options'],
                         ' -c structs.ledger_writer_contract=denomination-v1 -c structs.signer_contract=v022-1')

    def test_existing_database_does_not_automatically_acknowledge_transition(self):
        result = self.run_script(STRUCTS_PG_MIGRATION_MODE='main',
                                 STRUCTS_PG_MIGRATION_TARGET='', REGISTRY_EMPTY='f')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('--to', self.calls()[0]['args'])
        self.assertEqual(self.calls()[0]['options'], '')

    def test_contracts_are_explicit_and_preserve_existing_options(self):
        result = self.run_script(STRUCTS_PG_MIGRATION_TARGET='latest',
                                 STRUCTS_LEDGER_WRITER_CONTRACT='denomination-v1',
                                 STRUCTS_SIGNER_CONTRACT='v022-1', PGOPTIONS='-c lock_timeout=5s')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('--to', self.calls()[0]['args'])
        self.assertEqual(self.calls()[0]['options'],
                         '-c lock_timeout=5s -c structs.ledger_writer_contract=denomination-v1 -c structs.signer_contract=v022-1')

    def test_already_deployed_boundary_is_verified_without_reverting(self):
        result = self.run_script(REGISTRY_DEPLOYED='t')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls()[0]['args'], ['verify', '--target',
                         'db:postgres://test@localhost/test', '--from', BOUNDARY, '--to', BOUNDARY])

    def test_invalid_configuration_never_reaches_sqitch(self):
        for env in [{'STRUCTS_PG_REF': 'main'}, {'STRUCTS_PG_REF': ''},
                    {'STRUCTS_PG_MIGRATION_MODE': 'unknown'}, {'STRUCTS_PG_BRANCH': 'main'},
                    {'STRUCTS_PG_MIGRATION_MODE': 'branch'}, {'STRUCTS_PG_DIR': '/does-not-exist'},
                    {'STRUCTS_PG_MIGRATION_MODE': 'main', 'STRUCTS_PG_BRANCH': '--invalid'},
                    {'STRUCTS_PG_MIGRATION_TARGET': '--help'}, {'SQITCH_PG_CONNECTION': ''},
                    {'STRUCTS_SIGNER_CONTRACT': 'v021'}, {'STRUCTS_LEDGER_WRITER_CONTRACT': 'anything'}]:
            with self.subTest(env=env):
                self.assertNotEqual(self.run_script(**env).returncode, 0)
                self.assertEqual(self.calls(), [])

    def test_dirty_sources_are_refused(self):
        (self.checkout / 'migration.sql').write_text('modified\n')
        self.assertNotEqual(self.run_script().returncode, 0)
        self.assertEqual(self.calls(), [])

    def test_registry_connection_failure_is_not_empty_database(self):
        self.assertNotEqual(self.run_script(PSQL_EXIT='3').returncode, 0)
        self.assertEqual(self.calls(), [])

    def test_migration_failure_stops_the_recurring_loop(self):
        result = self.run_script('database-auto-migrate.sh', SQITCH_EXIT='7', AUTO_MIGRATE_SLEEP='120')
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual(len(self.calls()), 1)

    def test_verification_failure_is_not_success(self):
        result = self.run_script(REGISTRY_DEPLOYED='t', SQITCH_EXIT='6')
        self.assertEqual(result.returncode, 6, result.stderr)
        self.assertEqual(len(self.calls()), 1)


if __name__ == '__main__':
    unittest.main(verbosity=2)
