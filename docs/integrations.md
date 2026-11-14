# Deterministic integration interface

Paddock owns task execution, terminal sessions, worktrees, and recovery. A local
service dispatches through the structured CLI. The service account's OS identity
is the authenticated caller; `--requester` and `--requester-source` record
integration assertions, not authentication. Run each service under its own OS
account and create its own named Paddock identity.

```sh
paddock identity create remediation --profiles investigation,implementation
paddock run --repo project --identity remediation --profile investigation \
  --requester 'user-123' --requester-source 'incident-service' \
  --external-id 'case-456' --context-file context.json --idle-ttl 24h --json --detach \
  'Investigate the reported issue'
paddock tasks --json
paddock show TASK_ID --json
paddock output TASK_ID --limit 20 --json
paddock output TASK_ID --after CURSOR --limit 20 --json
paddock artifacts TASK_ID --json
paddock message TASK_ID 'Follow-up detail' --external-id 'followup-789' --json
```

`context.json` is a JSON object of string keys and values, limited to 64 KiB.
Use it for correlation and structured context; the task prompt carries work
instructions. Paddock snapshots the caller, requester, context, external ID,
identity, authenticated GitHub actor, profile, and capabilities in immutable
task provenance.
`--external-id` is an idempotency key scoped to the local caller and requester
source. A repeated dispatch with the same prompt and identity returns its prior
task; changed prompt, requester, context, or explicitly supplied repository,
base, runtime, and terminal choices are rejected. A restarted integration can scan
`tasks --json` for its external ID, then inspect `show --json` for status,
session, branch, worktree, errors, child tasks, and pending access requests.
For a live Codex task, `show --json` reads the saved native conversation and
reports `last_turn.state` (`none`, `inProgress`, `completed`, `failed`,
`interrupted`, or `unknown`). For Claude, Paddock reads only structural events
from the pinned native transcript inside OpenShell; a `turn_duration` following
the latest user prompt proves completion. A queued follow-up remains
`inProgress` until its own turn completes. A completed turn can coexist with a
running interactive terminal. Before a native session stops, Paddock saves its
last observed turn, or `unknown` if inspection fails, in the task record. Thus
`show --json` retains that result after
the sandbox stops and the calling service restarts. A resumed session clears
the prior snapshot before new work begins. An inspection failure is `unknown`
with an error, never inferred completion. The `blocked` flag means unresolved
OpenShell access
proposals exist; inspect `access_requests` and `last_turn` together because a
proposal can remain pending after the agent finishes a turn. Claude's initial
workspace trust dialog is a native approval and may show `last_turn.state:
none` until a human responds.
Launch preparation holds a live process lease. If the dispatching CLI dies
before recording a launch, a later status refresh marks the stale task
`interrupted` after two minutes; an active slow preparation stays `preparing`.
`artifacts --json` returns the task's commit IDs, untracked file names,
tracked patch (capped at 2 MiB), and PR URLs found by its exact branch through
the pinned GitHub App, including closed PRs and PRs without a watch. It reports
PR lookup errors separately from workspace or patch errors.
Terminal state is refreshed when tasks are listed or shown. Use `attach` to join
the existing terminal and `message` to deliver a native follow-up. A message
external ID is scoped to the authenticated caller and task. Paddock records
intent before delivery. A confirmed offline result is `pending` and can be
retried with the same ID. A crash or ambiguous delivery is `claimed` or
`uncertain` and is never replayed automatically; inspect the agent session
before sending a new message.

`output --json` reads the pinned OpenShell conversation without resuming it. It
returns public assistant messages and a `turn_finished` event when native history
records a terminal turn state. Codex reports `completed`, `failed`, or
`interrupted`; Claude reports `completed` when its transcript records
`turn_duration`. Paddock does not infer turn completion from a stopped process.
It omits prompts, hidden reasoning, tool
calls and results, and raw terminal output. A message has an ID, `turn_id`,
`phase` (`commentary`, `partial_answer`, or `final_answer`), and `text`.
Questions, progress, explanations, and communicated blockers are ordinary public
messages; an integration can relay the text using its own delivery rules. A
`turn_finished` event has `turn_id` and `state`; it is independent of whether
the task's interactive process is still running. A failed or interrupted turn
may have no final answer. Codex can retry its latest interrupted or failed turn
under the same ID. Paddock publishes that terminal event only after a later
turn starts or the session stops, so a saved cursor cannot skip a recovered
answer.

The result is a JSON object with `task_id`, `events`, `next_cursor`, and
`has_more`. `--limit` defaults to 20 and accepts 1–100 events. Each text event
is at most 64 KiB; long messages are split into ordered `part` and `parts`
events. Save `next_cursor` after delivering the page and pass it as `--after`
on the next poll. An empty page preserves the supplied cursor. Event IDs and
cursors remain stable within the saved native conversation, so a restarted
integration can retrieve missed output. A cursor from another task or
conversation, or one whose event has disappeared, is an error; integrations
should surface that condition rather than silently replay history. History
unavailable after sandbox removal is also an explicit error. Legacy host tasks
have no supported output reader. `message --json` continues to report delivery
only; pair its receipt with `output --json` to receive the agent's answer.

Named identity and integration runs require OpenShell. The trusted local caller
selects an allowed `investigation` or `implementation` profile. An unbacked
identity can work locally in OpenShell with no GitHub actor, token, or PR
capability. App-backed investigation receives a repository-scoped read token
and read-only GitHub policy. Investigation agents also run under a source-content
write guard with separate writable scratch and agent state. App-backed
implementation receives scoped write
permissions. The App private key stays on the host and the task receives a
short-lived installation token through its own OpenShell provider. The task
cannot choose a broader profile.

The CLI contains no application-specific requester interpretation. Integrations
must authenticate their own users and authorize requests before invoking it.

## Per-task idle TTL contract

An integration opts in at dispatch with `run --idle-ttl 24h`; omit the option
for unlimited interactive lifetime. This is generic policy, independent of
requester source. Durations must be positive Go durations, and JSON contains the
canonical `idle_ttl: "24h0m0s"`. The policy survives service restarts and native
resume. Delegated children opt in separately; they do not inherit it. Repeated
external-ID dispatches must supply the same TTL, including omission for a task
created without one. A changed/omitted policy is an error and never refreshes a
timer or changes the existing task.

`run --json`, `tasks --json` and `show --json` expose the task's `idle` checkpoint
when available:

```json
{
  "idle_ttl": "24h0m0s",
  "idle": {
    "generation": 1,
    "phase": "idle",
    "turn_id": "native-turn-id",
    "epoch": "opaque-runtime-epoch",
    "since": "2026-11-13T12:00:00Z",
    "deadline": "2026-11-14T12:00:00Z"
  }
}
```

`idle` can be absent before the poller's first check. `phase` is `waiting`
(no confirmed safe completed turn), `idle` (countdown established), `releasing`
(durable intent with an unconfirmed response), `draining` (native release
accepted), `expired` (stopped and pane cleanup complete), or `unsupported`
(policy retained without automatic shutdown). `error` describes why Paddock
retained a session. `drain_confirmed` records verified owner exit;
`pane_closed` records cleanup. Treat `epoch` as opaque, and compare generation
when reconciling observations. `since` and `deadline` are RFC3339 timestamps;
policy and observation fields are read-only. Polling never extends the deadline.

Expiry requires confirmed native completion and live safety checks; active work,
approval/input waits, pending delivery and unknown states retain the session.
The native conversation and task ID remain stable. After expiry,
`status: "stopped"` and `session.last_turn` retain the completion state, and
`output --json` continues serving stable event IDs/cursors from the saved public
output checkpoint. A later `message TASK_ID 'Follow-up' --external-id ... --json`
resumes that same conversation and delivers only the follow-up. The existing
message receipt contract is unchanged. An operation/release collision is offline
and retryable with the same `pending` external ID after cleanup; uncertain
outcomes must still be inspected. If operation-lock contention prevents the
initial message claim, the command fails before producing a receipt; retry the
same external ID. Once a receipt exists, confirmed offline outcomes are `pending`.

Automatic enforcement supports qualified OpenShell Codex runtimes and Claude
2.1.325 integration-only sessions. For Claude, integrations such as Arnold use
`run --agent claude --idle-ttl 24h --json --detach ...` and the existing `message`
contract unchanged. Submit all input through Paddock; direct TUI input and direct
native inbox writes are outside the TTL contract. Claude TTL launches disable
background/autonomous work and retain native foreground subagents and approvals.
The full TTL starts after confirmed native completion. Updated runtime images
and the v9 poller are required; Paddock
does not install them or restart services automatically. See
[verified shutdown, recovery and limitations](lifecycle.md#opt-in-idle-process-expiry).
