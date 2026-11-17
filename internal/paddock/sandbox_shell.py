#!/usr/bin/python3
"""Request a host shell or relay a parent message; no host CLI or credentials."""
import argparse
import hashlib
import json
import os
import pathlib
import secrets
import stat
import sys
import time

root = pathlib.Path('/sandbox/.paddock')


def read_regular(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ValueError('Expected a regular Paddock request or response file')
        data = source.read(limit + 1)
        if len(data) > limit:
            raise ValueError('Paddock request or response exceeds size limit')
        return data


def message(args):
    parser = argparse.ArgumentParser(prog='paddock message')
    parser.add_argument('target', help="Full ID of this task's registered parent")
    parser.add_argument('body')
    parser.add_argument('--external-id')
    parser.add_argument('--json', action='store_true')
    options = parser.parse_args(args)
    if not options.body.strip() or len(options.body.encode('utf-8')) > 65536 or '\x00' in options.body:
        parser.error('Message must contain 1–65536 UTF-8 bytes without NUL characters')
    key = options.external_id if options.external_id is not None else secrets.token_hex(12)
    if not key or len(key.encode('utf-8')) > 256:
        parser.error('--external-id must contain 1–256 UTF-8 bytes')
    request = hashlib.sha256(key.encode('utf-8')).hexdigest()[:24]
    content_hash = hashlib.sha256((options.target + '\x00' + options.body).encode('utf-8')).hexdigest()
    payload = {'id': request, 'target': options.target, 'body': options.body, 'external_id': key}
    pending = root / 'message-requests'
    pending.mkdir(mode=0o700, exist_ok=True)
    path = pending / request
    try:
        previous = json.loads(read_regular(path, 524288))
    except FileNotFoundError:
        previous = None
    if previous is not None and previous != payload:
        parser.error('--external-id already belongs to different content')
    if previous is None:
        if len(list(pending.iterdir())) >= 4:
            sys.exit('Four parent messages are pending. Check the host poller before sending more.')
        # Publish a complete file, never a partially written JSON request.
        temp = root / ('.message-' + secrets.token_hex(12))
        fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        try:
            with os.fdopen(fd, 'w') as out:
                json.dump(payload, out, ensure_ascii=False)
            try:
                os.link(temp, path)
            except FileExistsError:
                if json.loads(read_regular(path, 524288)) != payload:
                    parser.error('--external-id already belongs to different content')
        finally:
            temp.unlink(missing_ok=True)
    response = root / ('message-response-' + request)
    for _ in range(150):
        try:
            result = json.loads(read_regular(response, 131072))
        except FileNotFoundError:
            time.sleep(0.2)
            continue
        if result.get('id') != request or result.get('external_id') != key or result.get('content_hash') != content_hash:
            sys.exit('Invalid parent message acknowledgement; inspect the host before retrying.')
        # Keep the last receipt so a retry with the same key can inspect it.
        if options.json:
            print(json.dumps(result))
        else:
            print('Parent message ' + key + ': ' + result['status'])
            if result['status'] == 'pending':
                print('Queued for host retry; delivery is not yet confirmed. Reuse --external-id ' + key + ' for this message.')
            elif result.get('error'):
                print(result['error'])
        return 0 if result['status'] in ('delivered', 'pending') else 1
    result = {'id': request, 'external_id': key, 'status': 'pending',
              'error': 'No host acknowledgement. Request retained; check paddock github status on the host. Retry only with the same --external-id.'}
    if options.json:
        print(json.dumps(result))
    else:
        print('Parent message ' + key + ': ' + result['error'])
    return 1


if sys.argv[1:2] == ['message']:
    sys.exit(message(sys.argv[2:]))
if sys.argv[1:] != ['attach']:
    sys.exit("This sandbox shortcut supports: paddock attach; paddock message PARENT_TASK_ID 'message' [--external-id KEY] [--json]\nRun other Paddock commands on the host.")
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
