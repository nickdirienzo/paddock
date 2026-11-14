import importlib.machinery
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

idle = importlib.machinery.SourceFileLoader(
    'claude_idle', str(Path(__file__).with_name('paddock-claude-idle'))).load_module()


class ClaudeIdleTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.home = self.root / 'home'
        self.history = self.home / 'projects' / 'work' / 'conversation.jsonl'
        self.history.parent.mkdir(parents=True)
        self.child = Mock()
        self.child.poll.return_value = None
        self.fence = idle.ClaudeIdle(self.root, 4, 'conversation', '/work', self.home, self.child)
        self.request = {'op': 'status', 'generation': 4, 'thread': 'conversation', 'cwd': '/work'}
        self.events = []
        self.prompt('first')
        self.event(type='assistant', uuid='answer', message={'role': 'assistant', 'stop_reason': 'end_turn'})
        self.stop()
        self.complete()

    def event(self, **event):
        event.update(sessionId='conversation', cwd='/work')
        self.events.append(event)
        self.history.write_text(''.join(json.dumps(event) + '\n' for event in self.events))

    def prompt(self, identity):
        self.event(type='user', uuid=identity, message={'role': 'user', 'content': 'fixture'})

    def hook(self, kind, **fields):
        return self.fence.inspect(dict(self.request, op='hook', event={
            'session_id': 'conversation', 'cwd': '/work', 'hook_event_name': kind, **fields}))

    def stop(self, **fields):
        return self.hook('Stop', background_tasks=fields.get('background_tasks', []),
                         session_crons=fields.get('session_crons', []))

    def complete(self):
        self.event(type='system', subtype='stop_hook_summary', uuid='hooks',
                   hookErrors=[], preventedContinuation=False)
        self.event(type='system', subtype='turn_duration', uuid='duration',
                   pendingBackgroundAgentCount=0, pendingWorkflowCount=0)

    def confirm(self):
        return self.fence.inspect(self.request)

    def expire(self, state):
        return self.fence.inspect(dict(self.request, op='expire', epoch=state['epoch'], turn_id=state['turn_id']))

    def test_live_stop_and_successful_native_completion_are_both_required(self):
        stale = idle.ClaudeIdle(self.root, 4, 'conversation', '/work', self.home, self.child)
        with self.assertRaises(ValueError):
            stale.inspect(self.request)  # Saved history alone never qualifies.
        state = self.confirm()
        self.assertEqual('completed', state['state'])
        for _ in range(3):
            self.assertEqual(state, self.fence.inspect(self.request))
        self.assertEqual('draining', self.expire(state)['state'])
        self.expire(state)  # Repeated/lost responses never send another signal.
        self.child.terminate.assert_called_once()
        self.assertEqual('offline', self.fence.inspect(dict(self.request, op='delivery'))['state'])
        self.assertFalse((self.root / 'idle-stopped.json').exists())
        self.assertEqual(0, self.fence.exited(143))
        marker = json.loads((self.root / 'idle-stopped.json').read_text())
        self.assertEqual(state['epoch'], marker['epoch'])
        self.assertEqual(4, marker['generation'])

    def test_delivery_invalidates_before_native_write_and_old_idle_hook_is_insufficient(self):
        state = self.confirm()
        self.assertEqual('admitted', self.fence.inspect(dict(self.request, op='delivery'))['state'])
        self.hook('Notification', notification_type='idle_prompt')
        with self.assertRaises(ValueError):
            self.expire(state)
        self.child.terminate.assert_not_called()
        self.prompt('next')
        self.event(type='assistant', uuid='answer-next', message={'stop_reason': 'end_turn'})
        self.stop()
        self.complete()
        next_state = self.confirm()
        self.assertNotEqual(state['epoch'], next_state['epoch'])
        self.assertEqual('next', next_state['turn_id'])
        with self.assertRaises(ValueError):
            self.expire(state)

    def test_stop_hook_can_precede_transcript_flush(self):
        self.events.pop()  # Native turn_duration is still buffered.
        self.history.write_text(''.join(json.dumps(event) + '\n' for event in self.events))
        with self.assertRaises(ValueError):
            self.fence.inspect(self.request)
        self.complete()
        self.assertEqual('completed', self.fence.inspect(self.request)['state'])

    def test_active_tool_approval_input_failure_and_compaction_retain(self):
        for kind in ('UserPromptSubmit', 'PreToolUse', 'PermissionRequest', 'SubagentStart',
                     'StopFailure', 'PreCompact', 'ConfigChange'):
            with self.subTest(kind=kind):
                self.stop()
                state = self.confirm()
                self.hook(kind)
                self.hook('Notification', notification_type='idle_prompt')
                with self.assertRaises(ValueError):
                    self.expire(state)
        self.child.terminate.assert_not_called()

    def test_background_work_crons_and_missing_native_metadata_retain(self):
        for fields in ({'background_tasks': [{'type': 'monitor'}]},
                       {'session_crons': [{'id': 'later'}]},
                       {'background_tasks': None}, {'session_crons': None}):
            with self.subTest(fields=fields):
                self.stop(**fields)
                self.hook('Notification', notification_type='idle_prompt')
                with self.assertRaises(ValueError):
                    self.fence.inspect(self.request)

    def test_epoch_identity_and_history_change_reject_shutdown(self):
        state = self.confirm()
        for changed in ({'generation': 3}, {'thread': 'other'}, {'cwd': '/other'},
                        {'epoch': 'stale'}, {'turn_id': 'stale'}):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                request = dict(self.request, op='expire', epoch=state['epoch'], turn_id=state['turn_id'])
                request.update(changed)
                self.fence.inspect(request)
        self.event(type='queue-operation', operation='enqueue', content='queued')
        with self.assertRaises(ValueError):
            self.expire(state)
        self.child.terminate.assert_not_called()

    def test_failed_exit_or_lost_runtime_never_proves_expiry(self):
        state = self.confirm()
        self.expire(state)
        self.assertEqual(1, self.fence.exited(1))
        self.assertFalse((self.root / 'idle-stopped.json').exists())
        # A new launch cannot inherit a prior native notification or epoch.
        restarted = idle.ClaudeIdle(self.root, 5, 'conversation', '/work', self.home, self.child)
        with self.assertRaises(ValueError):
            restarted.inspect(dict(self.request, generation=5))

    def test_failed_interrupted_partial_or_unanchored_history_never_qualifies(self):
        state = self.confirm()
        self.event(type='assistant', uuid='error', isApiErrorMessage=True,
                   message={'stop_reason': 'end_turn'})
        self.complete()
        with self.assertRaises(ValueError):
            self.expire(state)
        with self.history.open('a') as stream:
            stream.write('{')
        with self.assertRaises(ValueError):
            self.confirm()


if __name__ == '__main__':
    unittest.main()
