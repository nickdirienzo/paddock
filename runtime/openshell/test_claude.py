"""Protocol-boundary tests; run on Linux (the runtime platform)."""
import importlib.machinery
import json
import os
from pathlib import Path
import socket
import tempfile
import threading
import unittest

message = importlib.machinery.SourceFileLoader('message', str(Path(__file__).with_name('paddock-claude-message'))).load_module()


@unittest.skipUnless(hasattr(socket, 'SO_PEERCRED'), 'Linux runtime peer credentials')
class InboxTest(unittest.TestCase):
    def test_idle_drain_rejects_delivery_before_native_write(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'claude-process.json').write_text(json.dumps({
                'pid': os.getpid(), 'conversation': 'saved', 'generation': 2, 'idle_protocol': 1}))
            with socket.socket(socket.AF_UNIX) as server:
                server.bind(str(root / 'idle-control.sock'))
                server.listen()
                requests = []
                def reject():
                    with server.accept()[0] as connection:
                        requests.append(json.loads(connection.makefile().readline()))
                        connection.sendall(b'{"state":"offline"}\n')
                worker = threading.Thread(target=reject)
                worker.start()
                self.assertEqual('offline', message.deliver(root, 'saved', 2, 'hello'))
                worker.join(1)
                self.assertEqual([{'op': 'delivery', 'generation': 2, 'thread': 'saved'}], requests)
            # No control socket means no admission, even with native metadata.
            self.assertEqual('offline', message.deliver(root, 'saved', 2, 'hello'))

    def test_native_admission_evidence(self):
        for admission in ('queued', 'held', 'refused'):
            with self.subTest(admission=admission), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                home = root / 'home'
                home.mkdir()
                (root / 'claude-process.json').write_text(json.dumps({'pid': os.getpid(), 'conversation': 'saved', 'generation': 2}))
                (home / 'projects' / 'test').mkdir(parents=True)
                log = home / 'projects' / 'test' / 'saved.jsonl'
                log.write_text('old enqueue evidence must not count\n')
                with socket.socket(socket.AF_UNIX) as server:
                    server.bind(str(home / 'paddock-inbox.sock'))
                    server.listen()
                    frames = []
                    def receive():
                        with server.accept()[0] as connection:
                            frame = json.loads(connection.makefile().readline())
                            frames.append(frame)
                            prefix = frame['message']['content'].splitlines()[0]
                            with log.open('a') as output:
                                if admission == 'queued':
                                    output.write(json.dumps({'type':'queue-operation','operation':'enqueue','sessionId':'saved','content':frame['message']['content']}) + '\n')
                                else:
                                    output.write('[cross-session-inbound] ' + admission + ': ' + prefix + '\n')
                    worker = threading.Thread(target=receive)
                    worker.start()
                    result = message.deliver(root, 'saved', 2, 'fix the PR', timeout=0.2, home=home)
                    worker.join(1)
                    self.assertEqual(result, 'queued' if admission == 'queued' else 'uncertain')
                    self.assertEqual(frames[0]['priority'], 'next')
                    self.assertNotIn('from_mode', frames[0])  # Never claim bypass permission parity.
                    self.assertEqual(frames[0]['message']['content'].splitlines()[1], 'fix the PR')

    def test_stale_generation_and_missing_inbox_are_retryable(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'claude-process.json').write_text(json.dumps({'pid': os.getpid(), 'conversation': 'saved', 'generation': 2}))
            self.assertEqual(message.deliver(root, 'other', 2, 'hello'), 'offline')
            self.assertEqual(message.deliver(root, 'saved', 1, 'hello'), 'offline')
            self.assertEqual(message.deliver(root, 'saved', 2, 'hello'), 'offline')


if __name__ == '__main__':
    unittest.main()
