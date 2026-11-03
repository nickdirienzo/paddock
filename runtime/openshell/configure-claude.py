#!/usr/bin/env python3
"""Give OpenShell sole refresh ownership of an explicitly selected Claude login.

Use a dedicated login, never the active host CLI's credentials. No Keychain
discovery. Credentials travel through child environment lookup, not argv/logs.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time


def configure(args):
    try:
        auth_path = Path(args.auth_file).resolve()
        if auth_path == Path.home() / '.claude' / '.credentials.json':
            raise ValueError()
        tokens = json.loads(auth_path.read_text())['claudeAiOauth']
        access, refresh = (tokens[k] for k in ['accessToken', 'refreshToken'])
        expiry = int(tokens['expiresAt'])
        if not access or not refresh or expiry <= time.time() * 1000:
            raise ValueError()
    except (OSError, ValueError, TypeError, KeyError):
        raise SystemExit('Select a fresh dedicated Claude OAuth login, not the host CLI login')
    command = [args.openshell, '--gateway', args.gateway, '--workspace', args.workspace]

    def run(step, tail, env=None):
        result = subprocess.run(command + tail, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if result.returncode:
            raise SystemExit(step + ' failed; inspect refresh status without printing credentials. '
                             'Do not reuse this login in another CLI once refresh is configured.')

    profile = str(Path(__file__).with_name('claude-provider.json'))
    run('Profile validation', ['profile', 'lint', '--file', profile])
    run('Profile import', ['profile', 'import', '--file', profile])
    env = os.environ.copy()
    env['CLAUDE_CODE_OAUTH_TOKEN'] = access
    run('Provider creation', ['provider', 'create', '--name', args.name,
                             '--type', 'paddock-claude', '--credential', 'CLAUDE_CODE_OAUTH_TOKEN'], env)
    env.pop('CLAUDE_CODE_OAUTH_TOKEN')
    env['PADDOCK_CLAUDE_REFRESH_TOKEN'] = refresh
    run('Refresh configuration', ['provider', 'refresh', 'configure', args.name,
        '--credential-key', 'CLAUDE_CODE_OAUTH_TOKEN', '--strategy', 'oauth2-refresh-token',
        '--credential-expires-at', str(expiry),
        '--material', 'client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e',
        '--secret-material-env', 'refresh_token=PADDOCK_CLAUDE_REFRESH_TOKEN'], env)
    run('Initial refresh', ['provider', 'refresh', 'rotate', args.name,
                           '--credential-key', 'CLAUDE_CODE_OAUTH_TOKEN'])
    print('Provider configured with automatic renewal. Do not reuse the imported login.')
    print('Set paddock runtime configure --claude-provider ' + args.name)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--openshell', default='openshell')
    parser.add_argument('--gateway', required=True)
    parser.add_argument('--workspace', default='default')
    parser.add_argument('--auth-file', required=True)
    parser.add_argument('--name', default='paddock-claude')
    configure(parser.parse_args())
