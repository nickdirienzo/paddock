#!/usr/bin/python3
"""Carry selected MCP messages over the task supervisor's existing exec pipe."""
import json
import os
from pathlib import Path
import queue
import socket
import socketserver
import sys
import threading

LIMIT = 16 * 1024 * 1024
mode, root = sys.argv[1], Path(sys.argv[2])
address = str(root / 'mcp.sock')

if mode == 'client':
    server = sys.argv[3]
    with socket.socket(socket.AF_UNIX) as sock:
        sock.connect(address)
        with sock.makefile('rwb') as connection:
            for line in sys.stdin.buffer:
                if len(line) > LIMIT:
                    sys.exit('MCP request exceeds bridge limit')
                request = json.loads(line)
                connection.write(json.dumps({'server': server, 'request': request}).encode() + b'\n')
                connection.flush()
                if 'id' in request:
                    reply = connection.readline(LIMIT + 1)
                    if not reply or len(reply) > LIMIT:
                        sys.exit('Paddock host MCP bridge disconnected; inspect before retrying')
                    sys.stdout.buffer.write(reply)
                    sys.stdout.buffer.flush()
    sys.exit(0)

if mode != 'serve':
    sys.exit('Unknown MCP bridge mode')

root.mkdir(mode=0o700, parents=True, exist_ok=True)
# The host process lease prevents concurrent supervisors for this task.
Path(address).unlink(missing_ok=True)
pending = {}
lock = threading.Lock()
sequence = 0

class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        global sequence
        while True:
            line = self.rfile.readline(LIMIT + 1)
            if not line or len(line) > LIMIT:
                return
            try:
                envelope = json.loads(line)
                request = envelope['request']
                if not isinstance(request, dict) or not isinstance(envelope['server'], str):
                    return
            except (KeyError, ValueError, TypeError):
                return
            reply = queue.Queue(maxsize=1)
            with lock:
                sequence += 1
                key = sequence
                if 'id' in request:
                    pending[key] = reply
                sys.stdout.write(json.dumps({'id': key, 'server': envelope['server'], 'request': request}) + '\n')
                sys.stdout.flush()
            if 'id' in request:
                try:
                    result = reply.get(timeout=660)
                    if result is None:
                        return
                    self.wfile.write(json.dumps(result).encode() + b'\n')
                    self.wfile.flush()
                except queue.Empty:
                    return
                finally:
                    with lock:
                        pending.pop(key, None)

class Server(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True
    request_queue_size = 32

with Server(address, Handler) as service:
    os.chmod(address, 0o600)
    threading.Thread(target=service.serve_forever, daemon=True).start()
    print(json.dumps({'ready': True}), flush=True)
    try:
        while True:
            line = sys.stdin.buffer.readline(LIMIT + 1)
            if not line or len(line) > LIMIT:
                break
            envelope = json.loads(line)
            with lock:
                target = pending.get(envelope['id'])
                if target is not None:
                    target.put_nowait(envelope['response'])
    finally:
        with lock:
            for target in pending.values():
                try:
                    target.put_nowait(None)
                except queue.Full:
                    pass
        service.shutdown()
        Path(address).unlink(missing_ok=True)
