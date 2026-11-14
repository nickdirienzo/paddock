"""Opt-in native qualification; uses a temporary home and a local fake model.

PADDOCK_IDLE_NATIVE_CODEX=/usr/local/bin/codex python3 -m unittest test_idle_native -v
No real login, model requests, user sessions, or installed services are used.
"""
import asyncio
import contextlib
import http.server
import json
import os
from pathlib import Path
import shutil
import shlex
import signal
import socket
import subprocess
import tempfile
import threading
import time
import unittest

from test_idle import idle

CODEX = os.environ.get('PADDOCK_IDLE_NATIVE_CODEX')


class Model(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.0'
    release = threading.Event()
    mode = 'answer'
    calls = []

    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        self.calls.append(body)
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()

        def event(kind, **fields):
            data = dict(type=kind, **fields)
            self.wfile.write(('event: ' + kind + '\ndata: ' + json.dumps(data) + '\n\n').encode())
            self.wfile.flush()

        event('response.created', response={'id': 'r-fixture', 'object': 'response', 'status': 'in_progress'})
        if self.mode == 'hold':
            self.release.wait(20)
        if self.mode == 'approval':
            names = [tool.get('name', '') for tool in body.get('tools', [])]
            name = next((name for name in names if name in ('exec_command', 'shell_command', 'shell')), None)
            if name is None:
                raise ValueError('native shell tool missing: ' + repr(names))
            args = {'sandbox_permissions': 'require_escalated', 'justification': 'Paddock fixture approval'}
            args['cmd' if name == 'exec_command' else 'command'] = 'printf APPROVAL_FIXTURE'
            if name == 'shell':
                args['command'] = ['sh', '-c', 'printf APPROVAL_FIXTURE']
            item = {'type': 'function_call', 'id': 'tool', 'call_id': 'call-fixture',
                    'name': name, 'arguments': json.dumps(args)}
            Model.mode = 'answer'
        else:
            item = {'type': 'message', 'id': 'answer', 'role': 'assistant', 'phase': 'final_answer',
                    'content': [{'type': 'output_text', 'text': 'PADDOCK_FIXTURE_OK'}]}
        event('response.output_item.added', output_index=0, item=item)
        event('response.output_item.done', output_index=0, item=item)
        event('response.completed', response={'id': 'r-fixture', 'object': 'response', 'status': 'completed',
              'output': [item], 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}})


@unittest.skipUnless(CODEX and os.sys.platform in ('linux', 'darwin'), 'opt-in native drain qualification')
class NativeDrainTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.root = Path(tempfile.mkdtemp(dir='/tmp'))
        self.addCleanup(shutil.rmtree, self.root, True)
        self.home = self.root / 'codex'
        self.home.mkdir()
        self.worktree = self.root / 'worktree'
        self.worktree.mkdir()
        self.model = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Model)
        self.model_thread = threading.Thread(target=self.model.serve_forever, daemon=True)
        self.model_thread.start()
        self.addCleanup(self.model.server_close)
        self.addCleanup(self.model.shutdown)
        Model.mode, Model.calls = 'answer', []
        Model.release.clear()
        (self.home / 'config.toml').write_text('''model = "fixture"
model_provider = "fixture"
[model_providers.fixture]
name = "fixture"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
''' % self.model.server_port)
        self.processes = []
        self.logs = []
        self.addCleanup(self.cleanup_processes)
        self.seq = 0
        await self.start_owner()

    def cleanup_processes(self):
        Model.release.set()
        for process in self.processes:
            if process.poll() is None:
                process.kill()  # disposable fixture process only
            process.wait()
        for log in self.logs:
            log.close()

    async def start_owner(self):
        (self.home / 'app-server-control').mkdir(exist_ok=True)
        self.native = str(self.home / 'app-server-control/app-server-control.sock')
        Path(self.native).unlink(missing_ok=True)
        self.log = open(self.root / 'native.log', 'a+')
        self.logs.append(self.log)
        env = {'PATH': os.environ['PATH'], 'HOME': str(self.root), 'CODEX_HOME': str(self.home),
               'RUST_LOG': 'warn', 'LANG': 'C.UTF-8'}
        self.owner = subprocess.Popen([CODEX, 'app-server', '--listen', 'unix://' + self.native],
                                      env=env, cwd=self.worktree, stdout=self.log, stderr=self.log)
        self.processes.append(self.owner)
        for _ in range(200):
            if Path(self.native).exists():
                break
            if self.owner.poll() is not None:
                self.fail((self.root / 'native.log').read_text())
            await asyncio.sleep(.05)
        from websockets.legacy.client import unix_connect
        self.ws = await unix_connect(self.native, max_size=32 << 20)
        self.addAsyncCleanup(self.ws.close)
        init = await self.call('initialize', {'clientInfo': {'name': 'paddock-fixture', 'version': '1'},
                                             'capabilities': {'experimentalApi': True}})
        self.assertTrue(any(v in init['userAgent'] for v in idle.QUALIFIED), init)
        await self.ws.send(json.dumps({'method': 'initialized'}))

    async def call(self, method, params):
        self.seq += 1
        await self.ws.send(json.dumps({'id': self.seq, 'method': method, 'params': params}))
        for _ in range(20000):
            data = json.loads(await asyncio.wait_for(self.ws.recv(), 15))
            if data.get('id') == self.seq:
                if 'error' in data:
                    raise ValueError(data['error'])
                return data['result']
        raise ValueError('native fixture response missing')

    async def begin(self, mode='answer'):
        result = await self.call('thread/start', {'cwd': str(self.worktree), 'approvalPolicy': 'on-request',
                                                'sandbox': 'read-only', 'model': 'fixture'})
        self.thread = result['thread']['id']
        Model.mode = mode
        return await self.call('turn/start', {'threadId': self.thread,
                                            'input': [{'type': 'text', 'text': 'fixture assignment'}]})

    async def completed(self):
        for _ in range(20000):
            frame = json.loads(await asyncio.wait_for(self.ws.recv(), 20))
            if frame.get('method') == 'turn/completed':
                self.assertEqual('completed', frame['params']['turn']['status'], frame)
                return frame
        self.fail('native completion missing')

    async def wait_exit(self):
        for _ in range(300):
            if self.owner.poll() is not None:
                return
            await asyncio.sleep(.05)
        self.fail('graceful native drain did not finish: ' + (self.root / 'native.log').read_text())

    @unittest.skipUnless(os.sys.platform == 'linux', 'Linux kernel peer pidfd qualification')
    async def test_idle_fence_release_and_same_conversation_resume(self):
        await self.begin()
        await self.completed()
        # Exercise the deployed Unix proxy/control listener and atomic peer
        # pidfd binding, not just an in-process Fence with a mock owner.
        previous = os.environ.get('CODEX_HOME')
        os.environ['CODEX_HOME'] = str(self.home)
        gateway = asyncio.create_task(idle.serve(self.root, 1))
        try:
            for _ in range(100):
                if (self.root / 'idle-ready.json').exists():
                    break
                if gateway.done():
                    await gateway
                await asyncio.sleep(.05)
            request = {'op': 'status', 'generation': 1, 'thread': self.thread, 'cwd': str(self.worktree)}
            state = await asyncio.to_thread(idle.client, self.root, request)
            self.assertEqual('completed', state['state'])
            response = await asyncio.to_thread(idle.client, self.root, dict(request, op='expire', epoch=state['epoch'], turn_id=state['turn_id']))
            self.assertEqual('draining', response['state'])
            await self.wait_exit()
            for _ in range(50):
                if (self.root / 'idle-stopped.json').exists():
                    break
                await asyncio.sleep(.05)
            self.assertTrue((self.root / 'idle-stopped.json').exists())
        finally:
            gateway.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await gateway
            if previous is None:
                os.environ.pop('CODEX_HOME', None)
            else:
                os.environ['CODEX_HOME'] = previous
        saved = self.thread
        await self.start_owner()
        resumed = await self.call('thread/resume', {'threadId': saved})
        self.assertEqual(saved, resumed['thread']['id'])
        await self.call('turn/start', {'threadId': saved, 'input': [{'type': 'text', 'text': 'follow-up only'}]})
        await self.completed()
        history = (await self.call('thread/read', {'threadId': saved, 'includeTurns': True}))['thread']
        self.assertEqual(2, len(history['turns']))
        # The second model request includes context and exactly one new user
        # message, not a replay of the original assignment as a new turn.
        self.assertEqual(2, len(Model.calls))

    @unittest.skipUnless(os.sys.platform == 'linux' and os.path.exists('/usr/local/bin/paddock-idle'),
                         'mounted runtime launcher qualification')
    async def test_interactive_launcher_records_exit_after_native_release(self):
        # A disposable controlling PTY runs the production interactive launcher.
        # No terminal keystrokes or synthetic approval answers are injected.
        home_config = self.home / 'config.toml'
        with home_config.open('a') as stream:
            stream.write('\n[projects.' + json.dumps(str(self.worktree)) + ']\ntrust_level = "trusted"\n')
        # The owner was started before this trust edit; restart just this
        # disposable fixture so both its owner and TUI load the fixture config.
        self.owner.send_signal(signal.SIGHUP)
        await self.wait_exit()
        await self.start_owner()
        launcher_log = open(self.root / 'launcher.log', 'w+')
        self.addCleanup(launcher_log.close)
        args = ['/usr/bin/python3', str(Path(__file__).with_name('paddock-agent')), '7', CODEX,
                '--remote', 'unix://' + str(self.root / 'idle-rpc.sock'),
                '--sandbox', 'read-only', '--ask-for-approval', 'on-request', '--', 'fixture assignment']
        env = {'PATH': os.environ['PATH'], 'HOME': str(self.root), 'CODEX_HOME': str(self.home),
               'PADDOCK_CONTROL_DIR': str(self.root), 'PADDOCK_IDLE_TTL': '1s',
               'TERM': 'xterm-256color', 'LANG': 'C.UTF-8',
               'PYTHONPATH': os.environ.get('PYTHONPATH', '')}
        process = subprocess.Popen(['script', '-q', '-e', '-c', shlex.join(args), '/dev/null'],
                                   stdin=subprocess.PIPE, stdout=launcher_log, stderr=launcher_log,
                                   env=env, cwd=self.worktree, start_new_session=True)
        self.processes.append(process)
        self.addCleanup(process.stdin.close)
        status = None
        for _ in range(300):
            if process.poll() is not None:
                self.fail('launcher exited early: ' + (self.root / 'launcher.log').read_text())
            loaded = await self.call('thread/loaded/list', {'limit': 100})
            thread = None
            for identity in loaded['data']:
                metadata = (await self.call('thread/read', {'threadId': identity, 'includeTurns': False}))['thread']
                if metadata.get('ephemeral') is not True:
                    thread = identity
                    break
            if thread:
                request = {'op': 'status', 'generation': 7, 'thread': thread, 'cwd': str(self.worktree)}
                try:
                    status = await asyncio.to_thread(idle.client, self.root, request)
                    if status.get('state') == 'completed':
                        break
                except (OSError, ValueError) as error:
                    if error.args and isinstance(error.args[0], dict):
                        raise
            await asyncio.sleep(.05)
        self.assertIsNotNone(status, (self.root / 'launcher.log').read_text())
        self.assertEqual('completed', status.get('state'), repr(status) + (self.root / 'launcher.log').read_text())
        turns = await self.call('thread/turns/list', {'threadId': thread, 'limit': 10, 'itemsView': 'notLoaded'})
        self.assertEqual(1, len(turns['data']))
        response = await asyncio.to_thread(idle.client, self.root, dict(request, op='expire',
                                           epoch=status['epoch'], turn_id=status['turn_id']))
        self.assertEqual('draining', response['state'])
        await self.wait_exit()
        for _ in range(200):
            if process.poll() is not None:
                break
            await asyncio.sleep(.05)
        self.assertEqual(0, process.poll(), (self.root / 'launcher.log').read_text())
        self.assertEqual({'generation': 7, 'code': 0}, json.loads((self.root / 'agent-exit.json').read_text()))
        self.assertTrue((self.root / 'idle-stopped.json').exists())
        # TUI title generation may make a separate ephemeral model call; the
        # pinned main conversation above has exactly the original single turn.

    async def test_graceful_drain_waits_for_active_work_and_rejects_new_turns(self):
        await self.begin('hold')
        # Ensure the model is executing, not just a stored completed turn.
        for _ in range(100):
            if Model.calls:
                break
            await asyncio.sleep(.05)
        self.owner.send_signal(signal.SIGHUP)
        await asyncio.sleep(.2)
        self.assertIsNone(self.owner.poll(), 'native drain interrupted active model work')
        with self.assertRaises(ValueError) as error:
            await self.call('turn/start', {'threadId': self.thread, 'input': [{'type': 'text', 'text': 'must reject'}]})
        self.assertIn('drain', str(error.exception).lower())
        # Repeated graceful-only signals must never force exit.
        self.owner.send_signal(signal.SIGHUP)
        await asyncio.sleep(.2)
        self.assertIsNone(self.owner.poll())
        Model.release.set()
        await self.wait_exit()

    async def test_graceful_drain_preserves_native_approval_wait(self):
        await self.begin('approval')
        approval = None
        for _ in range(20000):
            frame = json.loads(await asyncio.wait_for(self.ws.recv(), 20))
            if frame.get('method') == 'item/commandExecution/requestApproval':
                approval = frame
                break
            if frame.get('method') == 'turn/completed':
                self.fail('native approval was bypassed: ' + repr(frame))
        self.assertIsNotNone(approval)
        self.owner.send_signal(signal.SIGHUP)
        await asyncio.sleep(.2)
        self.assertIsNone(self.owner.poll(), 'native drain interrupted approval wait')
        await self.ws.send(json.dumps({'id': approval['id'], 'result': {'decision': 'decline'}}))
        await self.wait_exit()


if __name__ == '__main__':
    unittest.main()
