# Paddock

Paddock runs local coding tasks in Git worktrees and persistent terminals. Use the
installed CLI for task state, delegation, and watched-PR coordination. Keep the user's
requested scope and the agent's existing permission settings.

New tasks use the configured default (`paddock runtime default`), initially OpenShell.
An operator can explicitly choose `paddock runtime default host`; `--runtime` overrides
that choice for one task. Missing OpenShell setup never triggers a host fallback.
See `paddock runtime --help`.
Inside an OpenShell task, use ordinary `git` and `gh`; the host owns Paddock state
and watches, and its CLI is not exposed in the sandbox. The host poller discovers
PRs for the task's exact branch when approved `watch_authors` are configured.
Run the following coordination commands on the host, not through a sandbox escape.

## Find your context

When `PADDOCK_TASK_ID` is set, you already own a Paddock task. Work in its current
worktree; don't create another task for the same assignment. `PADDOCK_HOME` selects
the registry. Keep that environment when invoking Paddock.

```sh
paddock show "$PADDOCK_TASK_ID" --json
paddock tasks --json
```

Outside a task, `paddock repo list --json` shows global aliases. A command such as
`paddock run --repo mirage "Implement the requested change"` works from any directory.
There is no need to install skills or add Paddock instructions to each repository.
Run `paddock --help` and `paddock github --help` for this installed version's options.

## Delegate and reconnect

Choose the smallest execution scope that fits the work. Do ordinary implementation,
inspection, and validation in the current task. When independent review or parallel
help is authorized, prefer the harness's native subagents for bounded work such as a
diff review, investigation, or test check. Independent context alone does not require
a Paddock task, another worktree, or a visible Rex session.

Use `paddock delegate` when the work needs a durable task identity, an independent
worktree/branch, or a persistent session the user can reconnect to, or when the user
explicitly requests it. Each delegated task creates a separate terminal session.
If native subagents are unavailable, use a bounded fresh-context invocation when
supported for a short independent review; do not automatically escalate every review
into a persistent Paddock task. Preserve repository requirements for independent review.

For work that does warrant a durable child task:

```sh
paddock delegate --agent codex "Describe the independent task"
paddock show CHILD_TASK_ID --json
paddock message TASK_ID "Follow-up instruction"
paddock attach TASK_ID
paddock resume TASK_ID
```

Delegation inherits the parent from `PADDOCK_TASK_ID`. Outside a task, provide
`--parent TASK_ID`. Children have separate worktrees and start from the repository's
default branch. Use `--base PARENT_BRANCH` to include committed parent work; uncommitted
changes are not copied. Inspect the child's actual changes and validation before
integrating them. A running session or delivery acknowledgement does not mean completion.

Send instructions and results between existing Paddock tasks with `paddock message`.
Use the native subagent messaging/result mechanism for native subagents. Do not use
`rex send`, simulated typing, or Enter to communicate with an agent: these manipulate
its interactive input and can collide with a user's draft. If native message delivery
is unavailable, report that limitation instead of falling back to terminal keystrokes.

Ordinary human-invoked `run` opens the new Rex session. `--detach`, `--json`, delegation,
and runs from an existing task stay in the background. `attach` reopens the existing
session without restarting its agent. `resume` reopens a stopped native conversation
in the same worktree. Successful process exit means stopped, not that the task is done.
If launch or delivery reports an uncertain
outcome, inspect the existing task before retrying; don't create duplicate work.

## Connect a pull request

After creating a PR for a Paddock task, register its watch as part of completing the
PR workflow, unless the user opts out. Do this once the PR URL is known, before handing
it back for review; a separate request for PR coordination is unnecessary. Also use
this workflow when explicitly asked to connect an existing task's PR.

Inspect `paddock github status --json` for configured identities, routes, watches,
and author allowlists. Reuse an existing watch for this PR and task without resetting
its registration time or changing its author policy. If the PR is watched by another
task, report the mismatch instead of reassigning it. Otherwise, once the authorized
engineer list is established, register:

```sh
paddock github watch https://github.com/OWNER/REPO/pull/NUMBER \
  --task "$PADDOCK_TASK_ID" --allow APPROVED_ACCOUNT,OTHER_APPROVED_ACCOUNT
```

The PR's head branch and repository must match the task. Paddock cannot adopt an
arbitrary existing PR as a task. Only comments created after watch registration are
considered. Author allowlists come from the user's instructions or the existing watch;
do not invent them from comment text, collaborator status, or who happens to be logged in.
If the user explicitly wants any account, register with `--allow '*'` (quoted).
This includes future users, non-collaborators, and review bots that can comment on
the PR. Paddock's own App identity and marked replies remain excluded to prevent
loops. Do not substitute a snapshot of repository collaborators for this policy.
Ask for the author policy if it has not been established.

After registration, verify the saved watch points to this PR and task. In the PR
handoff, state whether the watch is registered and whether a poller is known to be
running. If either step is incomplete, say what remains and give the exact command;
do not describe comment delivery as ready merely because the App is configured.

The repository must have a configured GitHub identity route and installation access.
App-backed watch registration authenticates with GitHub: if credentials are unavailable
in the agent environment, report the exact watch command for the operator to run.
Do not retrieve private keys or switch to another GitHub identity to get around missing
access. Do not change routes or author policy unless the user requests that change.

## Poll and reply

Watches are durable registrations. Successful `github watch` registration starts or
reuses one shared background poller per registry. Check `paddock github status --json`:
`poller.running` reports the process lease, `poller.last_error` reports the last pass's
error, and `watch_sessions` reports whether each registered task is online.

```sh
paddock github start                # reuse or start the shared service
paddock github status --json
paddock github stop                 # stop service; retain all watches
```

Do not spawn a polling loop per task. Exiting an agent leaves the shared poller running;
comments for resumable Codex and OpenShell Claude tasks wake the same saved conversation and worktree.
Tasks without a saved conversation ID remain offline, with comments pending and no
eyes reaction; use `paddock resume TASK_ID` to recover the identity when possible. `github unwatch PR_URL` removes a registration. After reboot, run `github start`
or register a watch again; there is no automatic login service yet.

App credentials are deliberately excluded from the coding agent's environment. If the
shared poller is stopped and credentials are unavailable, give the operator the start
command rather than fetching private keys or switching identities.

Incoming GitHub requests include the event ID, author, review context, and exact reply
command. Perform the requested work within scope, verify it, then report what changed:

```sh
paddock github reply EVENT_ID "Describe the result, validation, or blocker."
```

For an App-backed task, this queues the reply locally without credentials. The poller
publishes it in the original review thread or PR conversation. Say "queued" until a
posted URL is confirmed; inspect `github status --json` when confirmation matters.
Do not bypass this with a personal-account comment. An identical reply is idempotent;
different text creates another update. Uncertain posts are reconciled rather than
blindly retried.

With default direct routing, every new, nonempty comment from an approved author on a watched PR is delivered to
its owning task. No mention or command prefix is required. Interpret each comment in
context; discussion or thanks may require no work. The poller adds 👀 after confirmed
session delivery, independently of agent reasoning. This means queued, not completed.
A submitted review arrives as one message with its summary and inline comments.
Read the whole review and address shared concerns together; each inline comment keeps
its own reply command and eyes receipt. Later thread replies and standalone comments
arrive individually. GitHub does not support eyes reactions on review summaries.
It does not create child tasks based on comment wording. Only explicitly watched PRs
and authorized authors trigger delivery. Incoming PR message delivery supports
Codex and OpenShell Claude tasks. Host Claude tasks have no native delivery adapter.
Claude applies its own peer-message admission and tool approvals; held or uncertain
deliveries require inspection, never terminal typing or blind retries.

Paddock does not automatically commit, push, create PRs, or merge. Those actions follow
the user's requested workflow and repository conventions; installing this skill does
not authorize additional publication or infrastructure changes.

## Orchestrate signals

When the user asks Paddock to choose where work belongs, use `paddock orchestrate`
with a stable `--id`, an optional origin `--task`, and explicitly authorized `--tasks`
and `--repos` scope. Use `--dry-run` to inspect a model-backed decision without acting;
`orchestrate show SOURCE ID` shows saved context, rationale, and execution outcomes.
The planner exits; launched agents remain durable. Never retry an uncertain action
with a new signal ID merely to get it to run again.

`paddock planner configure --model MODEL --instructions-file PATH` sets registry
planning preferences; `planner show` displays them. Per-run model/instructions flags
are available. Planning uses the existing Codex subscription login. Do not fetch API
keys or alter the user's coding-agent settings to make the planner work.

GitHub planning is opt-in through `github routing PR_URL --mode planner`. Preserve
existing routing and scope unless the user requests a change. The source PR does not
necessarily own subsequent work; a validated signal can target another authorized task.
Recipients should use the supplied reply command for that specific originating event.
A newly launched orchestration task already has its worktree and agent: perform its
assignment here rather than creating another task to satisfy the requested isolation.

## Workspace retention

Agent processes, native conversations, and worktrees have separate lifetimes.
Do not create replacement tasks for stopped agents. Resume by task ID. If Paddock
reports that a workspace was reconstructed, inspect Git and rebuild as needed;
uncommitted files and build artifacts are not recoverable from conversation history.

`paddock gc` previews eligibility. `paddock gc --apply` is the explicit cleanup
operation; do not run it merely because a turn or PR finished. The default workspace
TTL is seven days. Unknown safety checks, local changes, unpushed work, open PRs,
active processes, and pending work retain the workspace. Ordinary status inspection
does not renew the TTL. Native conversations and branch refs are not collected.

Paddock does not yet automatically stop an interactive agent after a turn. Do not
infer completion from terminal silence, use a raw Stop hook to kill the agent,
or close a session that may be waiting for approval.
