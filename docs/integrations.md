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
  --external-id 'case-456' --context-file context.json --json --detach \
  'Investigate the reported issue'
paddock tasks --json
paddock show TASK_ID --json
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
running interactive terminal. An inspection failure is `unknown` with an error,
never inferred completion. The `blocked` flag means unresolved OpenShell access
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
