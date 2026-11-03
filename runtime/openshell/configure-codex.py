#!/usr/bin/env python3
"""Import an explicitly selected, dedicated Codex login into an OpenShell provider.

Use a separate CODEX_HOME/login: the gateway becomes the refresh-token owner.
Secrets travel through child environment lookup, never argv or stdout.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser()
parser.add_argument('--openshell', default='openshell')
parser.add_argument('--gateway', required=True)
parser.add_argument('--workspace', default='default')
parser.add_argument('--auth-file', required=True)
parser.add_argument('--name', default='paddock-codex')
args = parser.parse_args()
command = [args.openshell, '--gateway', args.gateway, '--workspace', args.workspace]
profile = str(Path(__file__).with_name('codex-provider.json'))
# Error messages from upstream credential commands may contain secrets. Expose
# only which step failed, never their captured output.
def run(step, tail, env=None):
    result = subprocess.run(command + tail, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if result.returncode:
        raise SystemExit(step + ' failed; inspect provider configuration without printing credentials')

try:
    tokens = json.loads(Path(args.auth_file).read_text())['tokens']
    access, refresh, account = (tokens[k] for k in ['access_token', 'refresh_token', 'account_id'])
    if not access or not refresh or not account:
        raise ValueError()
except (OSError, ValueError, KeyError):
    raise SystemExit('Auth file must contain a dedicated Codex OAuth login')
run('Profile validation', ['profile', 'lint', '--file', profile])
run('Profile import', ['profile', 'import', '--file', profile])
env = os.environ.copy()
env['CODEX_AUTH_ACCESS_TOKEN'] = access
run('Provider creation', ['provider', 'create', '--name', args.name, '--type', 'paddock-codex', '--credential', 'CODEX_AUTH_ACCESS_TOKEN'], env)
env.pop('CODEX_AUTH_ACCESS_TOKEN')
env['CODEX_AUTH_REFRESH_TOKEN'] = refresh
run('Refresh configuration', ['provider', 'refresh', 'configure', args.name,
    '--credential-key', 'CODEX_AUTH_ACCESS_TOKEN', '--strategy', 'oauth2-refresh-token',
    '--material', 'client_id=app_EMoamEEZ73f0CkXaXp7hrann',
    '--secret-material-env', 'refresh_token=CODEX_AUTH_REFRESH_TOKEN'], env)
run('Initial refresh', ['provider', 'refresh', 'rotate', args.name, '--credential-key', 'CODEX_AUTH_ACCESS_TOKEN'])
print('Provider configured. Set paddock runtime configure --codex-provider ' + args.name)
print('Non-secret account selector: --codex-account-id ' + account)
