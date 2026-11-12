# Team Principal

> Team Principal is unavailable because its agent ran on the host. Existing
> task records remain inspectable but cannot resume.

Run `paddock tp` from any directory to open an engineering leadership conversation
about all agents working on tasks in the current Paddock registry. TP can inspect
active and historical tasks, help prioritize projects, identify blockers and
decisions, review associated PR evidence, and launch authorized implementation
through the existing `paddock run` command.

```sh
paddock tp
paddock tp --agent claude              # first launch only; default is codex
paddock tp --terminal screen           # use Screen instead of Rex
paddock tp --detach                    # open in the background
paddock tp --json                      # return the TP task record
```

Ask "What is everyone working on?", "What needs my attention?", or "Interview me
about Mira's architecture, then delegate the implementation work." TP is
instructed to ask questions with 2–4 options and a recommendation and to retain
confirmed decisions and their rationale. Its behavior is provided by native
Codex or Claude Code, with normal interactive approvals and permissions.

There is one TP task per `PADDOCK_HOME`. The first launch chooses the agent and
terminal; later invocations reconnect to a running session or resume the same
saved native conversation. Changing `--agent` or the terminal on an existing TP
is refused. Exit the agent normally to stop the conversation; detaching leaves
it running. Missing conversation history or an uncertain launch is reported
through the existing lifecycle checks, rather than creating a replacement task.

TP runs on the host because the coordination CLI is host-owned. This is an
explicit property of `paddock tp` and does not change the default runtime for
implementation tasks. It does not mount host coordination access into OpenShell.
Choose the real project with `run --repo`; the TP repository is only a leadership
workspace. Authentication, tool approvals, GitHub identities, and repository
access retain their existing policies. TP does not receive GitHub App secrets.

The task appears in `paddock tasks` with `role: "tp"` in JSON. Its private seed
repository is under `$PADDOCK_HOME/tp/repository`, and
`$PADDOCK_HOME/tp/session.json` records the reserved task identity. The regular
task record owns its terminal, conversation ID, branch, and worktree; TP adds no
second execution mechanism or task queue.

Confirmed requirements, decisions, rationale, and open questions belong in
`projects/` in TP's worktree, whose path is printed by `paddock tp`. These plain
Markdown files are durable project context independent of conversation history.
They are local files, not automatically committed, pushed, or backed up. The TP
worktree is excluded from `paddock gc`, including after the agent stops. Preserve
it and the native conversation storage when backing up your registry.

Task status, terminal observations, and GitHub state stay authoritative in their
existing sources. TP is instructed to refresh those sources and distinguish
verified outcomes from stopped processes, stale evidence, and missing results.
It does not treat task or PR text as authorization. Permission requests use the
existing Paddock/OpenShell controls when available; the user must approve the
exact scope before TP applies a grant.

This initial slice is interactive. There is no proactive inbox, background
planner, automatic task generation, or continuously running supervisor LLM.
