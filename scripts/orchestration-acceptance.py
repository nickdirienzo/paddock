#!/usr/bin/env python3
"""Live acceptance: real subscription-backed planning and durable Paddock agents.

Run explicitly: python3 scripts/orchestration-acceptance.py --root /private/tmp/paddock-live
Uses the built CLI and a connected Rex server. Creates two local Git repositories,
four durable agent sessions, and JSON evidence under root. Leaves those sessions
available for inspection. No PRs, pushes, or external comments are created.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time

parser = argparse.ArgumentParser()
parser.add_argument('--root', required=True)
parser.add_argument('--paddock', default='bin/paddock')
parser.add_argument('--model')
args = parser.parse_args()
root = Path(args.root).resolve()
root.mkdir(parents=True, exist_ok=True)
cli = str(Path(args.paddock).resolve())
env = dict(os.environ, PADDOCK_HOME=str(root / 'state'))
# Never inherit the invoking coding task as the identity of acceptance sessions.
env.pop('PADDOCK_TASK_ID', None)
env.pop('PADDOCK_PARENT_TASK_ID', None)

def command(argv, **kwargs):
    result = subprocess.run(argv, env=env, text=True, capture_output=True, **kwargs)
    if result.returncode:
        raise RuntimeError(f'{argv}: {result.stderr}\n{result.stdout}')
    return result.stdout

def paddock(*argv):
    return command([cli, *argv], timeout=600)

def save(name, value):
    (root / f'{name}.json').write_text(json.dumps(value, indent=2) + '\n')

def repository(name, text):
    path = root / name
    if not path.exists():
        path.mkdir()
        command(['git', '-C', str(path), 'init', '-b', 'main'])
        (path / 'README.md').write_text(text)
        command(['git', '-C', str(path), 'add', 'README.md'])
        command(['git', '-C', str(path), '-c', 'user.name=Paddock acceptance', '-c',
                 'user.email=acceptance@example.test', 'commit', '-m', 'Acceptance fixture'])
    paddock('repo', 'add', name, str(path))
    paddock('repo', 'trust', name)
    return path

api_repo = repository('garage-api', '# Garage API\nHealth endpoint response: {"status":"ok"}.\n')
docs_repo = repository('garage-docs', '# Garage documentation\nUser-facing documentation for the Garage API.\n')

# Persist IDs immediately; rerunning this script reuses the existing tasks.
def owner(name, repo, prompt):
    file = root / f'{name}.json'
    if file.exists():
        return json.loads(file.read_text())
    task = json.loads(paddock('run', '--repo', repo, '--terminal', 'rex', '--json', prompt))
    # run --json emits Task directly.
    save(name, task)
    return task

api = owner('api-owner', 'garage-api',
    'Own Garage API implementation work for an orchestration acceptance test. '
    'Read README.md, report ready, and await follow-up. Do not edit or commit yet. '
    'Do not open PRs or start other agents. Keep this session available.')
docs = owner('docs-owner', 'garage-docs',
    'Own user documentation for the Garage API for an orchestration acceptance test. '
    'Read README.md, report ready, and await documentation requests. Do not edit or commit yet. '
    'Do not open PRs or start other agents. Keep this session available.')

# Check actual terminal output, not elapsed time or merely task metadata, for readiness.
def capture(task):
    s = task['session']
    return command([s['terminal_path'], '--autostart=false', 'capture', '-s', s['id'],
                    '-b', s['block_id'], '--trim', '--unwrap'], timeout=20)

for name, task in [('api-owner', api), ('docs-owner', docs)]:
    deadline = time.monotonic() + 180
    while True:
        text = capture(task)
        (root / f'{name}-capture.txt').write_text(text)
        # A native thread is stronger evidence than a process label. Delivery will
        # resolve it through the Codex app-server before accepting any message.
        if 'ready' in text.lower() and ('Worked for' in text or 'Ask Codex' in text):
            break
        if time.monotonic() > deadline:
            raise RuntimeError(f'{name} did not become ready; inspect saved capture')
        time.sleep(2)

scope = ['--task', api['id'], '--tasks', docs['id'], '--repos', 'garage-api,garage-docs']
if args.model:
    scope += ['--model', args.model]

signals = [
    ('irrelevant', 'Lovely weather today. This is unrelated chatter, no request or action needed.', 'ignore'),
    ('existing-specialist',
     'The Garage API health response needs user documentation. Find the existing documentation '
     'owner, inspect its session context to confirm it is available, and ask it to add a short '
     'health-response example to README.md. No API implementation change is needed. '
     'Do not commit, push, open PRs, or launch another agent.', 'message'),
    ('independent-child',
     'Create a durable child task related to this Garage API task, with its own worktree and '
     'Codex session, to write HEALTH-TEST-PLAN.md describing three checks for the health response. '
     'The parent must remain available for other API work. This explicitly requires a persistent '
     'child task, not a message to the parent. Do not commit, push, or open PRs.', 'delegate'),
    ('separate-work',
     'Start a separate top-level task in the garage-docs repository to draft CONTRIBUTING.md '
     'with a three-step contributor workflow. This is independent of the API PR and must have '
     'its own worktree and Codex session, without a parent. Leave the existing docs owner on '
     'its current health-response documentation. Do not commit, push, or open PRs.', 'create_task'),
    ('reply-only',
     'Without assigning or starting any work, tell me which existing agent owns Garage '
     'user documentation and whether its session is available. Inspect current context first.', 'reply'),
]
results = {}
for name, body, expected in signals:
    signal_id = 'acceptance-' + name
    result = json.loads(paddock('orchestrate', '--id', signal_id, *scope, '--json', body))
    save(name, result)
    assert result['status'] == 'done', result
    kinds = [item['action']['kind'] for item in result['outcomes']]
    assert kinds == [expected], (name, kinds)
    if expected == 'message':
        assert result['outcomes'][0]['action']['task_id'] == docs['id'], result
        assert any(c['request']['kind'] == 'session' and not c.get('error')
                   for c in result['context']), 'model did not obtain session context'
    before = json.loads(paddock('tasks', '--json'))
    repeated = json.loads(paddock('orchestrate', '--id', signal_id, *scope, '--json', body))
    after = json.loads(paddock('tasks', '--json'))
    assert repeated == result, 'replay changed the signal decision/outcomes'
    assert {t['id'] for t in before} == {t['id'] for t in after}, 'replay created tasks'
    results[name] = {'signal': result['signal'], 'status': result['status'],
                     'outcomes': result['outcomes'], 'context_reads': len(result['context']),
                     'replay_unchanged': True}
    print(f'PASS {name}: {expected}; replay unchanged', flush=True)

all_tasks = json.loads(paddock('tasks', '--json'))
assert len(all_tasks) == 4, all_tasks
for t in all_tasks:
    assert t['status'] == 'running' and t['terminal']['state'] == 'running', t
child_id = results['independent-child']['outcomes'][0]['created_task_id']
new_id = results['separate-work']['outcomes'][0]['created_task_id']
by_id = {t['id']: t for t in all_tasks}
assert by_id[child_id]['parent_id'] == api['id']
assert not by_id[new_id].get('parent_id')
assert by_id[new_id]['repo'] == str(docs_repo)
assert by_id[new_id]['related_task_id'] == api['id']
for name, task in [('api-owner-final', api), ('docs-owner-final', docs),
                   ('child-final', by_id[child_id]), ('independent-final', by_id[new_id])]:
    (root / f'{name}-capture.txt').write_text(capture(task))
save('tasks-final', all_tasks)
save('acceptance-summary', results)
print('PASS orchestrators exited; all four durable agent sessions remain running', flush=True)
print(f'Evidence: {root}', flush=True)
