import asyncio
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

loader = importlib.machinery.SourceFileLoader('paddock_idle', str(Path(__file__).with_name('paddock-idle')))
spec = importlib.util.spec_from_loader(loader.name, loader)
idle = importlib.util.module_from_spec(spec)
loader.exec_module(idle)


class Native:
    def __init__(self):
        self.status = 'idle'
        self.turn = 'turn'
        self.turn_state = 'completed'
        self.queue = []
        self.goal = None
        self.terminals = []
        self.loaded = ['conversation']
        self.version = 'codex/0.159.2'
        self.cwd = '/worktree'
        self.calls = []
        self.hook = None

    def result(self, method, params):
        self.calls.append(method)
        if self.hook:
            self.hook(method)
        if method == 'initialize':
            return {'userAgent': self.version}
        if method == 'thread/loaded/list':
            return {'data': self.loaded}
        if method == 'thread/read':
            return {'thread': {'id': params['threadId'], 'cwd': self.cwd,
                              'status': {'type': self.status}, 'turns': [
                                  {'id': self.turn, 'status': self.turn_state}]}}
        if method == 'thread/goal/get':
            return {'goal': self.goal}
        if method == 'thread/turns/list':
            return {'data': [{'id': self.turn, 'status': self.turn_state}]}
        if method == 'thread/queue/list':
            return {'data': self.queue}
        if method == 'thread/backgroundTerminals/list':
            return {'data': self.terminals, 'nextCursor': None}
        raise ValueError(method)

    def connect(self):
        native = self

        class Connection:
            async def __aenter__(self):
                return self

            async def __aexit__(self, *args):
                pass

            async def send(self, data):
                self.request = json.loads(data)

            async def recv(self):
                return json.dumps({'id': self.request['id'], 'result': native.result(
                    self.request['method'], self.request.get('params'))})

        return Connection()


class IdleFenceTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.native = Native()
        self.fence = idle.Fence(self.temporary.name, 3, '/unused', 99)
        self.fence.connect = self.native.connect
        self.request = {'op': 'status', 'generation': 3, 'thread': 'conversation', 'cwd': '/worktree'}

    async def expire(self):
        state = await self.fence.inspect(self.request)
        return await self.fence.inspect(dict(self.request, op='expire', epoch=state['epoch'], turn_id=state['turn_id']))

    async def test_completed_idle_and_repeatable_no_force(self):
        with mock.patch.object(idle.signal, 'pidfd_send_signal', create=True) as send, \
                mock.patch.object(self.fence, 'confirm_exit', new=mock.AsyncMock()):
            state = await self.expire()
            self.assertEqual('draining', state['state'])
            await self.fence.inspect(self.request)
            await self.fence.inspect(dict(self.request, op='expire'))
            send.assert_called_once_with(99, idle.signal.SIGHUP)
            marker = json.loads((Path(self.temporary.name) / 'idle-drain.json').read_text())
            self.assertEqual(state['epoch'], marker['epoch'])
            self.assertEqual(3, marker['generation'])

    async def test_owner_exit_racing_with_signal_is_confirmed_without_retry(self):
        with mock.patch.object(idle.signal, 'pidfd_send_signal', create=True,
                               side_effect=ProcessLookupError) as send, \
                mock.patch.object(idle.select, 'select', return_value=([99], [], [])):
            state = await self.expire()
            self.assertEqual('draining', state['state'])
            await asyncio.sleep(0)
            marker = json.loads((Path(self.temporary.name) / 'idle-stopped.json').read_text())
            self.assertEqual(state['epoch'], marker['epoch'])
            send.assert_called_once()

    async def test_active_approval_input_unknown_and_no_completed_turn_retain(self):
        for status in ['active', 'notLoaded', 'systemError', 'unknown']:
            self.native.status = status
            with self.assertRaises(ValueError):
                await self.expire()
        self.native.status = 'idle'
        for status in ['inProgress', 'failed', 'interrupted', 'unknown']:
            self.native.turn_state = status
            with self.assertRaises(ValueError):
                await self.expire()

    async def test_pending_native_work_and_goals_retain(self):
        for field, value in [('queue', [{'message': 'pending'}]), ('goal', {'status': 'active'}),
                             ('goal', {'status': 'blocked'}), ('goal', {'status': 'budgetLimited'}),
                             ('terminals', [{'pid': 42}]), ('loaded', []), ('version', 'codex/0.1.0'),
                             ('cwd', '/other')]:
            before = getattr(self.native, field)
            setattr(self.native, field, value)
            with self.assertRaises(ValueError, msg=field):
                await self.expire()
            setattr(self.native, field, before)

    async def test_unknown_kernel_and_stale_generation_retain(self):
        self.fence.pidfd = None
        with self.assertRaises(ValueError):
            await self.expire()
        with self.assertRaises(ValueError):
            await self.fence.inspect(dict(self.request, generation=2))

    async def test_polling_does_not_change_epoch(self):
        initial = self.fence.epoch
        for method in idle.READS:
            self.fence.client('client', {'id': method, 'method': method})
            self.fence.server('client', {'id': method, 'result': {}})
        await self.fence.inspect(self.request)
        self.assertEqual(initial, self.fence.epoch)
        self.fence.client('client', {'id': 1, 'method': 'turn/start'})
        self.assertNotEqual(initial, self.fence.epoch)
        with self.assertRaises(ValueError):
            await self.fence.inspect(self.request)

    async def test_inflight_delivery_and_approval_response_retain(self):
        self.fence.server('client', {'id': 1, 'method': 'item/commandExecution/requestApproval'})
        with self.assertRaises(ValueError):
            await self.expire()
        epoch = self.fence.epoch
        self.fence.client('client', {'id': 1, 'result': {'decision': 'accept'}})
        self.assertNotEqual(epoch, self.fence.epoch)
        self.assertFalse(self.fence.requests)
        self.fence.server('client', {'id': 2, 'method': 'item/tool/requestUserInput'})
        with self.assertRaises(ValueError):
            await self.expire()
        self.fence.uncertain = True
        with self.assertRaises(ValueError):
            await self.expire()

    async def test_delivery_arrives_during_idle_inspection(self):
        state = await self.fence.inspect(self.request)
        self.native.hook = lambda method: self.fence.client('client', {'id': 1, 'method': 'thread/queue/add'})
        with mock.patch.object(idle.signal, 'pidfd_send_signal', create=True) as send:
            with self.assertRaises(ValueError):
                await self.fence.inspect(dict(self.request, op='expire', epoch=state['epoch'], turn_id=state['turn_id']))
            send.assert_not_called()
        self.assertFalse(self.fence.draining)

    async def test_work_between_checkpoint_and_shutdown_resets_epoch(self):
        state = await self.fence.inspect(self.request)
        self.fence.client('client', {'id': 2, 'method': 'thread/queue/add'})
        self.fence.server('client', {'id': 2, 'result': {}})
        with mock.patch.object(idle.signal, 'pidfd_send_signal', create=True) as send:
            with self.assertRaises(ValueError):
                await self.fence.inspect(dict(self.request, op='expire', epoch=state['epoch'], turn_id=state['turn_id']))
            send.assert_not_called()

    async def test_native_activity_during_final_inspection(self):
        state = await self.fence.inspect(self.request)
        self.native.hook = lambda method: self.fence.server('client', {
            'method': 'turn/started', 'params': {'threadId': 'conversation'}})
        with mock.patch.object(idle.signal, 'pidfd_send_signal', create=True) as send:
            with self.assertRaises(ValueError):
                await self.fence.inspect(dict(self.request, op='expire', epoch=state['epoch'], turn_id=state['turn_id']))
            send.assert_not_called()


try:
    import websockets
except ImportError:
    websockets = None


@unittest.skipIf(websockets is None, 'install python3-websockets for wire-level gateway tests')
class GatewayWireTests(unittest.IsolatedAsyncioTestCase):
    async def test_rpc_and_native_approvals_pass_unchanged(self):
        from websockets.legacy.client import unix_connect
        from websockets.legacy.server import unix_serve
        # macOS Unix socket paths have a short limit.
        with tempfile.TemporaryDirectory(dir='/tmp') as root:
            native_path, proxy_path = root + '/native', root + '/proxy'
            frames = []
            approval = {'id': 'approval', 'method': 'item/commandExecution/requestApproval',
                        'params': {'command': 'fixture'}}

            async def native(ws, _path):
                request = await ws.recv()
                frames.append(json.loads(request))
                await ws.send(json.dumps({'id': 1, 'result': {}}))
                await ws.send(json.dumps(approval))
                response = await ws.recv()
                frames.append(json.loads(response))
                await ws.send(json.dumps({'method': 'fixture/done'}))

            fence = idle.Fence(root, 3, native_path)
            async with unix_serve(native, native_path):
                async with unix_serve(fence.proxy, proxy_path):
                    async with unix_connect(proxy_path) as client:
                        request = {'id': 1, 'method': 'turn/start', 'params': {'approvalPolicy': 'on-request'}}
                        await client.send(json.dumps(request))
                        self.assertEqual({'id': 1, 'result': {}}, json.loads(await client.recv()))
                        self.assertEqual(approval, json.loads(await client.recv()))
                        with self.assertRaises(ValueError):
                            await fence.inspect({'op': 'status', 'generation': 3})
                        response = {'id': 'approval', 'result': {'decision': 'decline'}}
                        await client.send(json.dumps(response))
                        await client.recv()
            self.assertEqual([request, response], frames)


if __name__ == '__main__':
    unittest.main()
