# Conversations, processes, and warm workspaces

A task owns a branch and workspace. Its native Codex/Claude conversation survives
process exit; stopping a process does not mean the work is done. New successful
exits record `stopped` rather than `completed`. Existing records remain readable.
The seven-day workspace TTL is independent of conversation lifetime.

```sh
paddock resume TASK_ID                # same native conversation and workspace
paddock message TASK_ID "Continue…"   # wakes a stopped Codex task if needed
paddock lifecycle show
paddock lifecycle configure --worktree-ttl 168h
paddock gc                            # preview; no deletions
paddock gc --apply                    # check again, then remove eligible worktrees
```

Resume uses explicit `codex resume ID` / `claude --resume ID`, with the recorded
native agent home. It never uses `--last`, `--continue`, or forks the conversation.
The original assignment is not replayed. Codex identity discovery starts alongside
the supervisor; Claude receives a preassigned UUID at first launch. Older Codex
tasks can discover an unambiguous native identity during explicit resume.
Missing/ambiguous native history fails visibly; retain the native agent home along
with Paddock's registry. Paddock does not delete or relocate native history.

If an initial OpenShell launch failed with `trusted MCP bridge requires host
Codex` before the agent started, remove the obsolete host MCP grant and run
`paddock resume TASK_ID` to finish starting that same task. This specific recorded
startup failure permits the initial assignment to run in the existing workspace.
It does not permit replaying a task with a saved conversation or a previous resume.

The existing GitHub poller wakes `resumable` Codex and Claude tasks for authorized incoming
work. The same task/operation lock serializes resume, message delivery, and GC; a
supervisor holds a process lease until exit. Launch generations reject stale
supervisors. Existing signal IDs and delivery receipts still prevent duplicate
delivery. Failed wake-ups before queue delivery remain pending. OpenShell Codex
and Claude support native message delivery. Claude requires a runtime image with
its private home-directory inbox; older guarded sessions cannot resume under a
newer investigation guard.

## Retention and conservative GC

`last_activity_at` records launch/resume, incoming message delivery attempts, and
process exit. Running processes always prevent collection; their final exit starts
a fresh retention period. Metadata updates, status queries, and polling do not
extend retention. Legacy tasks without this clock begin a fresh retention period
on their first `gc --apply` rather than being deleted based on an unreliable age.

GC retains workspaces if any of these checks fail or are unknown:

- The path, repository, and branch still match the task.
- The retention period has elapsed and no supervisor/launch is active.
- The terminal has no running process; `lsof` reports no process using the path.
- Git reports no tracked/untracked changes, including dirty submodules.
- HEAD is reachable from the live remote upstream branch. No upstream, a deleted
  upstream, unavailable ancestry, or failed authentication means retain.
- No open PR exists for the branch in known GitHub remotes, fork parents, or watched
  repositories, using configured identities. Other Git hosts are currently retained.
- There are no pending/uncertain signals, GitHub events, or queued replies.
- Native conversation identity/history remains recoverable.

`lsof` must be available and produce a complete result. Platforms without that
inspection capability retain worktrees. Paddock operation/process locks protect its
own actors; manual filesystem edits and unrelated processes should be quiescent
while applying GC. Git performs its own final dirty/locked-worktree checks.

Ignored files are protected by default, including `.env`. Name only directories
whose ignored contents can be rebuilt:

```sh
paddock lifecycle configure --disposable-directories node_modules,.next,build
paddock lifecycle configure --disposable-directories none
```

This never permits deletion of tracked or non-ignored untracked changes. GC uses
`git worktree remove` without force. It retains branch refs and records HEAD before
removal, so a crash after removal still leaves a recovery checkpoint. A Git-locked
worktree remains protected by Git even if other preview checks passed.

Investigation tasks with a root-owned checkout keep that checkout in their
task-specific image. Per-worktree GC reports them ineligible; the image and
sandbox remain available for native resume. The collector does not remove the
immutable image layer separately.

## Recovery

Collected worktrees are reconstructed at the original path from the retained task
branch and checked against the saved commit, not recreated from the original base.
A moved/missing retained branch requires explicit inspection. A manually deleted
worktree can be recreated from its current task branch; an exact stale registration
can be replaced, but a locked registration or a branch in another checkout blocks
recovery. No unrelated worktree metadata is pruned.

The resumed agent receives a recovery notice identifying the commit and explaining
that caches and uncommitted filesystem state were not restored. Ordinary warm
resume leaves all files and build artifacts in place. Conversation history is not
a filesystem backup.

## Opt-in idle process expiry

```sh
paddock run --repo project --agent codex --idle-ttl 24h --json --detach \
  'Investigate the reported issue'
```

There is no idle TTL by default, including for existing tasks. `run` and
`delegate` accept `--idle-ttl` as a positive Go duration (`24h`, `90m`, `1h30m`).
Zero, negative, overflowing, unitless, and day-suffix durations are errors. The
policy is saved as canonical `idle_ttl` (for example `24h0m0s`). It is independent
of the seven-day workspace retention clock and never removes a branch, workspace,
native conversation, or output history.

Claude tasks support the same flag for **integration-only** sessions: submit all
work through `paddock message` or Paddock's integration delivery paths, and do not
type into the native TUI or use its inbox directly. Viewing the TUI is supported.
Paddock keeps subagents in the foreground and disables background sessions,
background shell execution, session cron tasks, MCP tasks, Monitor, automatic idle
compaction, model-proposed goals and automatic usage-limit continuation. These
restrictions apply on launch and resume only when a TTL is set. Foreground
subagents and native approval dialogs remain available. See
[Claude's environment variables](https://code.claude.com/docs/en/env-vars).

Children do **not** inherit a parent's TTL. Supply `delegate --idle-ttl` for each
child that should expire; each child's timer and native owner are independent.
A repeated dispatch using the same external ID must include the same effective
TTL (equivalent spellings are accepted). Omitting or changing it is a dispatch
conflict, never a policy update, launch, or timer refresh. No requester source has
special treatment.

The existing runtime poller checks opted-in tasks every five seconds; work across
many tasks or slow native reads can delay enforcement. It starts a full countdown
when it first confirms a completed native turn **and** live idle safety. It does
not count from task creation, terminal silence, metadata updates, or status polls.
The same completed turn and runtime epoch retain their saved deadline across
poller restarts. Admitted new work invalidates that epoch; active, blocked,
pending-delivery, and unknown states clear the countdown. A new completed turn
must establish a fresh full period. Failed and interrupted turns do not qualify.

Expiry requires no unresolved access proposals, pending/uncertain message
receipts, orchestration signals, GitHub events or replies. All loaded native
threads must be idle, with no queued messages, outstanding native RPCs, approval
or input requests, continuing/blocked goals, or background terminals. Incomplete
inspection retains the process. A stopped/missing terminal or saved completed
history alone never authorizes termination.

### Verified Codex shutdown

TTL Codex launches retain the interactive TUI and native approval policy. A
sandbox-local WebSocket gateway passes native requests and responses unchanged
and fences admission for that launch. Paddock's TUI and queue delivery use that
gateway. The host operation lock serializes expiry, delivery intent, message
submission, resume and GC. The gateway invalidates idle on new work before it
waits for the admission gate, so work arriving during the final inspection causes
expiry to be rejected. Launch generation, conversation, completed turn, workspace
and runtime epoch must all match the checkpoint.

With admission fenced, the gateway requests the native owner's graceful-only
`SIGHUP` drain through a kernel peer pidfd. The native drain rejects new work and
waits for admitted work, including approval waits, to finish. It is a second
runtime barrier, rather than a check followed by a blind process kill. Repeated
`SIGHUP` cannot force native shutdown. For Codex idle release, Paddock never
uses `SIGTERM`, a second forceable shutdown, an expiry timeout kill, or daemon-stop
to terminate model work. After the pinned native owner actually exits, the launcher may terminate
its disconnected TUI frontend. Paddock then confirms launcher exit, stops only the
task's sandbox, and closes only its already-exited Rex block. Sibling tasks and
user-added panes in the shared session remain intact.

The host saves release intent and a public-output checkpoint before requesting
drain. Accepted releases and confirmed native exit are reconciled after lost
responses or supervisor/poller restarts; recovery never blindly resends a signal.
Task state becomes `stopped`, with `idle.phase: expired` after cleanup. A later
`paddock message TASK_ID ...` resumes the same saved conversation with its native
home, branch, workspace and history, and queues only the new message. It does not
replay the initial assignment. A message colliding with an operation lock or a
release in progress is safely offline; use its existing `pending` receipt and
retry the same external ID once cleanup finishes. `claimed`/`uncertain` receipts
still require inspection before retry.

`output --json` serves the accepted expiry checkpoint while the sandbox is
stopped. Existing event IDs and cursors are preserved. Resume returns to the live
native reader for subsequent output. Native history itself remains in the task's
sandbox; the checkpoint contains only the existing public output representation,
not prompts, hidden reasoning or tool results.

This path is qualified against Codex 0.159.2 in Linux with a temporary native home
and local model fixture. Native active-work and approval-wait drain is also
qualified against Codex 0.166.0 on macOS. Tests exercise the real interactive
launcher in a disposable PTY, temporary title-generation helpers, actual native
completion, active-turn and approval-wait drain, repeated graceful signals,
rejection of new turns, and same-conversation follow-up resume.
Ephemeral native helpers cannot have persisted goals or queues; they still must
pass live idle and background-work checks. See the [native goal implementation](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/app-server/src/request_processors/thread_goal_processor.rs).
The gateway accepts native versions 0.159.2
and 0.166.0, whose drain/admission implementation is pinned in the
[Codex source](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/app-server/src/lib.rs)
and [native admission barrier](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/app-server/src/turn_admission.rs).
Unknown versions, missing native APIs, uninspectable runtime state, and kernels
without `SO_PEERPIDFD` retain the session. All supported client work must go
through Paddock's gateway; manually bypassing it through the private native
socket is outside the TTL contract.

### Claude integration-only shutdown

Claude TTL launches require Claude Code 2.1.325 and `paddock-claude-idle` in the
runtime image. The launcher owns the native child and a private admission socket.
Its native Stop hook must report no background work or scheduled tasks. The
native transcript must then confirm successful completion of all Stop hooks and
the completed turn boundary, with no later queued request or native activity.
Saved history alone never qualifies: each launch requires a fresh live Stop
observation. The full requested TTL begins when the poller confirms that turn;
polling never extends an established deadline. This also works after resume,
without relying on Claude's idle desktop notifications.

Incoming delivery invalidates the observation before writing to Claude's native
inbox. Native prompt, tool, approval, compaction and failure hooks invalidate it
too. The host task operation lock serializes every integration delivery with
expiry and resume. At expiry the launcher rechecks the exact generation,
conversation, workspace, turn and epoch, closes admission, persists release intent,
and sends one `SIGTERM` to its own native child for graceful shutdown. It never
uses a forced timeout kill or repeats the signal. Native exit must be confirmed
before sandbox and Rex-pane cleanup, using the same durable markers and public
output checkpoint as Codex. A later integration message resumes the exact saved
conversation and queues only the new work.

This contract depends on integration-only input and the launch restrictions above;
it does not cover manual TUI input, direct inbox writes, custom autonomous hooks,
or processes launched outside Claude's tracked foreground work. Missing hooks,
background work, pending input/approvals, changed history, failed exits and unknown
state retain the session. Claude's SDK-only `CLAUDE_CODE_EXIT_AFTER_STOP_DELAY` is
not used. See [native Stop hooks](https://code.claude.com/docs/en/hooks#stop).

### Deployment and agent limitations

Opt-in launches require an updated runtime image containing `paddock-idle`,
`paddock-claude-idle` and `python3-websockets`, plus the Paddock v9 runtime poller.
An older poller is reported as incompatible; upgrades do not silently restart it.
Existing sessions are not retrofitted. Claude 2.1.286 remains accepted for launches
without a TTL; TTL launches on that version fail before starting the agent and
require a rebuilt image. No new resident host service is introduced.

Native drains that cannot finish, or cleanup whose ownership/process state is
unknown, are retained for inspection without forced termination.
