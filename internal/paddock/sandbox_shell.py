#!/usr/bin/python3
"""Request a host shell for this sandbox; no host commands or credentials."""
import os
import pathlib
import secrets
import sys
import time

if sys.argv[1:] != ["attach"]:
    sys.exit("This sandbox shortcut supports only: paddock attach\nRun other Paddock commands on the host.")
root = pathlib.Path('/sandbox/.paddock')
pending = root / 'shell-requests'
pending.mkdir(mode=0o700, exist_ok=True)
if len(list(pending.iterdir())) >= 4:
    sys.exit('A host shell request is already pending. Check Rex and the host poller.')
request = secrets.token_hex(12)
fd = os.open(pending / request, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, 'w') as out:
    out.write(request)
print('Opening a host shell in Rex...', flush=True)
response = root / ('shell-response-' + request)
for _ in range(150):
    try:
        fd = os.open(response, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd) as f:
            result = f.read(4096)
        response.unlink()
        if result == 'opened':
            print('Host shell opened in the live sandbox worktree. Use open <path> directly.')
            sys.exit(0)
        sys.exit(result)
    except FileNotFoundError:
        time.sleep(0.2)
sys.exit('No host acknowledgement. Inspect Rex before retrying; check paddock github status on the host.')
