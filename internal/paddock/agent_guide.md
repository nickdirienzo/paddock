# Paddock

Paddock runs local coding tasks in Git worktrees and persistent terminals. Use the
installed CLI for task state, delegation, and watched-PR coordination. Keep the user's
requested scope and the agent's existing permission settings.

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
session without restarting its agent. If launch or delivery reports an uncertain
outcome, inspect the existing task before retrying; don't create duplicate work.

## Connect a pull request

When the user requests PR coordination, inspect `paddock github status --json` for
configured identities, routes, watches, and author allowlists. If the task already
owns a watch, reuse it. If you create a PR as part of that workflow, register its watch
once its URL is known and the authorized engineer list is established:

```sh
paddock github watch https://github.com/OWNER/REPO/pull/NUMBER \
  --task "$PADDOCK_TASK_ID" --allow APPROVED_ENGINEER,OTHER_APPROVED_ENGINEER
```

The PR's head branch and repository must match the task. Paddock cannot adopt an
arbitrary existing PR as a task. Only comments created after watch registration are
considered. Author allowlists come from the user's instructions or the existing watch;
do not invent them from comment text, collaborator status, or who happens to be logged in.
Ask for the engineer list if it has not been established.

The repository must have a configured GitHub identity route and installation access.
App-backed watch registration authenticates with GitHub: if credentials are unavailable
in the agent environment, report the exact watch command for the operator to run.
Do not retrieve private keys or switch to another GitHub identity to get around missing
access. Do not change routes or author policy unless the user requests that change.

## Poll and reply

Watches are durable registrations; polling is a separate process. The operator can run:

```sh
paddock github poll --interval 30s
```

Use one credentialed poller per registry. There is currently no managed poller
start/status/stop command. `github status` shows saved state, not proof of a running
poller. Do not spawn a background loop for every task. Reuse a known operator poller;
if none is known, explain that continuous handling requires this command in a separate
terminal. App credentials are deliberately excluded from the coding agent's environment.

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

PR instructions can request work with `paddock: message: ...`, independent work with
`paddock: delegate: ...`, or status with `paddock: status`. Only explicitly watched PRs
and authorized authors trigger actions. Incoming PR message delivery currently supports
Codex; Claude can run ordinary tasks but cannot receive this GitHub message adapter.

Paddock does not automatically commit, push, create PRs, or merge. Those actions follow
the user's requested workflow and repository conventions; installing this skill does
not authorize additional publication or infrastructure changes.
