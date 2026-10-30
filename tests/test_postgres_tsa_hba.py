"""Run against the Linux runtime image; no PostgreSQL server is changed."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class TsaHbaTests(unittest.TestCase):
    def apply(self, root):
        script = Path(__file__).resolve().parents[1] / 'scripts/postgres-common.sh'
        result = subprocess.run(['bash', '-c', 'source "$1"; tsa_test_conf="$2"; pg_conf_dir() { echo "$tsa_test_conf"; }; postgres_apply_tsa_hba',
            'test-hba', str(script), str(root)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_inherits_tls_scope_and_options_before_catchall_once(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root, 'pg_hba.conf')
            original = '# policy\nhostssl structs structs_webapp 10.12.0.0/16 scram-sha-256 clientcert=verify-ca\nhost all all 0.0.0.0/0 reject\n'
            path.write_text(original)
            path.chmod(0o640)
            self.apply(root)
            first = path.read_text()
            self.assertIn('hostssl structs structs_tsa 10.12.0.0/16 scram-sha-256 clientcert=verify-ca', first)
            self.assertLess(first.index('structs_tsa'), first.index('host all all'))
            self.assertEqual(path.stat().st_mode & 0o777, 0o640)
            self.apply(root)
            self.assertEqual(first, path.read_text())

    def test_preserves_explicit_policy_and_does_not_invent_access(self):
        for original in ('hostssl structs structs_tsa 10.0.0.1/32 reject\nhostssl structs structs_webapp 0.0.0.0/0 trust\n',
                         'host structs structs_webapp 10.0.0.0/8 scram-sha-256\n'):
            with self.subTest(policy=original), tempfile.TemporaryDirectory() as root:
                path = Path(root, 'pg_hba.conf')
                path.write_text(original)
                self.apply(root)
                self.assertEqual(path.read_text(), original)
                self.assertEqual(list(Path(root).iterdir()), [path])


if __name__ == '__main__':
    unittest.main()
