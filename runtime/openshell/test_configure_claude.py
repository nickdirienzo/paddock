import argparse
import importlib.machinery
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

helper = importlib.machinery.SourceFileLoader('configure', str(Path(__file__).with_name('configure-claude.py'))).load_module()


class ImportTest(unittest.TestCase):
    def test_only_gateway_receives_refresh_material(self):
        with tempfile.TemporaryDirectory() as directory:
            auth = Path(directory) / 'auth.json'
            expiry = int(time.time() * 1000) + 600000
            auth.write_text(json.dumps({'claudeAiOauth': {'accessToken': 'secret-access', 'refreshToken': 'secret-refresh', 'expiresAt': expiry}}))
            args = argparse.Namespace(auth_file=str(auth), openshell='openshell', gateway='test', workspace='default', name='test-claude')
            calls = []
            def run(command, **kwargs):
                calls.append((command, dict(kwargs.get('env') or {})))
                return argparse.Namespace(returncode=0)
            with patch.object(helper.subprocess, 'run', side_effect=run), patch('builtins.print'):
                helper.configure(args)
            self.assertEqual(len(calls), 5)
            for command, env in calls:
                self.assertNotIn('secret-access', ' '.join(command))
                self.assertNotIn('secret-refresh', ' '.join(command))
            self.assertEqual(calls[2][1]['CLAUDE_CODE_OAUTH_TOKEN'], 'secret-access')
            self.assertNotIn('PADDOCK_CLAUDE_REFRESH_TOKEN', calls[2][1])
            self.assertEqual(calls[3][1]['PADDOCK_CLAUDE_REFRESH_TOKEN'], 'secret-refresh')
            self.assertNotIn('CLAUDE_CODE_OAUTH_TOKEN', calls[3][1])
            self.assertIn(str(expiry), calls[3][0])
            self.assertIn('rotate', calls[4][0])

    def test_expired_login_never_creates_provider(self):
        with tempfile.TemporaryDirectory() as directory:
            auth = Path(directory) / 'auth.json'
            auth.write_text(json.dumps({'claudeAiOauth': {'accessToken': 'access', 'refreshToken': 'refresh', 'expiresAt': 1}}))
            with patch.object(helper.subprocess, 'run') as run, self.assertRaises(SystemExit):
                helper.configure(argparse.Namespace(auth_file=str(auth)))
            run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
