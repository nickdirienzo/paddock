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

## Automatic process shutdown: not enabled

New Rex panes close when the agent actually exits. Resuming older tasks closes only
Paddock's already-exited pane, preserving user-added panes. Paddock never infers turn
completion from terminal inactivity.

Investigation used Codex CLI 0.159.2, its shared daemon 0.166.0, and Claude Code
2.1.286. Native resume was exercised for both agents after process exit, with the
same conversation ID and recalled prior-turn context. Real Rex fixture tests cover
the supervisor, foreground process, independent task exits, and pane closure.

Codex exposes `turn/completed` and status flags for approval/user-input waits, but
the shared daemon does not necessarily own a Rex TUI's thread. A read-only live
probe reported an existing TUI conversation as `notLoaded`; reading stored history
does not establish current runtime safety. No verified atomic idle-and-release
interface was found for these independently hosted TUIs. See the
[Codex app-server protocol](https://developers.openai.com/codex/app-server).

Claude's `Stop` hook may be followed by continuation from another hook, and the
transcript may not contain the final message yet. It is not a safe process-kill
trigger. See [Claude's Stop hook](https://code.claude.com/docs/en/hooks#stop).

Consequently, this increment implements durable resume and explicit GC, while
leaving automatic turn-end process termination disabled. Headless native runs were
used only to validate persistence/resume; production sessions retain their native
interactive UI and approval behavior. No approval-policy changes, idle timers,
new resident service, or forced idle-process kills were added.
