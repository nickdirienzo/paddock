# Paddock reference

Commands, configuration, and operating details. For an introduction and the
basic workflow, start with the [README](../README.md).

For named agent identities, GitHub App registration, and deterministic service
dispatch, see [GitHub identity onboarding](github.md#create-a-named-paddock-identity-and-register-its-app)
and [the integration interface](integrations.md).

## Setup

Requirements: Git, a configured OpenShell gateway, agent image, model provider,
a named identity, and either a running local Rex server or GNU Screen. A GitHub
App is required for GitHub work; local-only identities need no App. macOS is
the tested development platform; the terminal adapters also target Unix
environments. Native Windows terminal execution is not implemented (use WSL).
Building a Windows binary alone does not provide Windows terminal support.

Development tooling is pinned with [mise](https://mise.jdx.dev/):

```sh
mise trust
mise install
mise run build
mise run check
install -m 755 bin/paddock ~/.local/bin/paddock
~/.local/bin/paddock skill setup
```

Ensure `~/.local/bin` is on your PATH. Go is only needed to build; the resulting
executable has no language runtime or database dependency. Keep the executable
at a stable path while tasks are running because host task supervisors use it.

```sh
cd /path/to/any/git/repository
paddock run "Implement feature X" --agent codex
paddock tasks
paddock show <task-id>
paddock attach <task-id>
```

For a multiline goal, pass `-`, paste the prompt, and press Ctrl-D to send EOF:

```sh
paddock run --repo project -
```

Prompts can also be read from a file or pipeline:

```sh
paddock run --repo project --prompt-file goal.md
pbpaste | paddock run --repo project -
```

The directory must have an existing Git commit. No repository registration is
needed. Default agent: `codex`. Default terminal: use a reachable local Rex
server, otherwise GNU Screen. Use `--terminal rex` or `--terminal screen` to
choose explicitly. An explicit backend failure never silently launches another
session elsewhere. Ordinary `paddock run` also selects the new session in the
connected Rex app when Remote Control is enabled. Use `--detach` to leave it in
the background. JSON output and delegation stay in the background; Screen
continues to launch detached.

Bare repositories are supported too. Point at the bare repository itself; Paddock creates a separate task worktree from it:

```sh
paddock run "Implement feature X" --repo ~/code/example/project.git
```

A plain directory named `project` next to `project.git` is not automatically a checkout. Paddock follows Git's repository discovery and does not guess sibling repository names. An existing linked worktree is also a valid starting directory.

### Interactive task launcher

`paddock new` opens a terminal form with a repository picker, agent selector,
multiline prompt, and Start task button. Registered repositories are listed by
name; the current project is preselected when available. **Other repository…**
accepts a registered name or a path, including `~/` and bare repositories.

```sh
paddock new
paddock new --repo project --agent claude
paddock rex setup
rex config reload
```

In Rex, press **Option-Command-N** or choose **New Paddock Task** from the command
palette. The launcher opens as a floating pane over the active tab, using the
focused terminal's working directory when Rex can report it. It passes the
invoking Rex client's ID so the task opens in the same app. Enable Rex's Remote
Control setting to allow session selection.

Use arrows to choose a repository or agent, Tab / Shift-Tab to move between
fields, Enter for a newline in the prompt, and Escape or Ctrl-C to cancel. Both
Codex and Claude Code are shown in the agent list; all agent tasks use OpenShell. Selecting **Cancel** or pressing
Escape or Ctrl-C creates no task. Launch errors remain visible until
dismissed. If session selection fails after creation, the launcher shows the
existing task's attach command; it does not launch again.

`new` accepts `--repo`, `--agent`, `--runtime`, `--base`, `--terminal`, and
`--client`. `--runtime` accepts only `openshell`. The Rex action explicitly uses Rex; a direct `paddock new` uses the same
terminal selection as `run`. Use `run` for scripts and pipelines.

Setup records the executable's absolute path. Install Paddock at its stable
location before running setup, and rerun setup if that location changes. It
also records the setup shell's PATH and explicit Paddock tool overrides so
Rex can find the Paddock executable when started from the GUI.
Run setup from a shell where your agents are available, and rerun it after
changing their installation paths. Other shell environment variables and
credentials are not copied into the launcher configuration. Setup updates only
a marked section of Rex's `init.lua`, preserving surrounding user
configuration and file permissions. It refuses malformed markers and symlinked
configuration files.

```sh
paddock rex setup --key 'ctrl+alt+n'
paddock rex remove
rex config reload
```

The default shortcut is `alt+cmd+n`; choose an unused shortcut with `--key` if
needed. Setup and removal accept `--config path`; otherwise they honor
`REX_CONFIG`, then `$XDG_CONFIG_HOME/rex/init.lua`, then `~/.config/rex/init.lua`.
Reload Rex using the same configuration after making a change.

### Agent discovery across repositories

Run `paddock skill setup` once per user on each machine. It installs a small discoverable
skill for [Codex](https://learn.chatgpt.com/docs/build-skills#where-codex-loads-local-skills)
at `~/.agents/skills/paddock/SKILL.md` and for
[Claude Code](https://code.claude.com/docs/en/skills#choose-where-skills-load) at
`~/.claude/skills/paddock/SKILL.md`. No repository-level skill or `AGENTS.md` changes
are required. Use `--agent codex` or `--agent claude` to target just one.

```sh
paddock skill status --json
paddock skill show
paddock skill remove
```

The agent sees the skill's name and description and can load it when relevant. The skill
invokes `paddock skill show` using the executable's installed absolute path, so upgrading
that binary updates the guide too. Re-run setup if the executable moves. Start a new
agent session if an existing session has not discovered the new skill. You can also
request `$paddock` in Codex or `/paddock` in Claude Code explicitly.

Host tasks receive their task ID, registry environment, and a direct
command to read this same guide. That startup reference also works without global skill
discovery. Setup creates no hooks and changes no agent approval settings. It updates
only Paddock-managed skill files, refuses conflicting unmanaged files or symlinks, and
removal preserves other files in the skill directory.

Sandboxed tasks receive runtime-specific instructions to use ordinary `git` and `gh`. The host poller discovers their PRs when runtime `--allow` authors are configured; the host registry and credentials are not exposed inside the sandbox.

The guide makes watch registration part of creating a PR for a Paddock task, using
the approved author list, and asks agents to report watch and poller readiness when
handing back the PR. It also covers delegation and queued replies.
Skills teach the workflow; installing one does not start a poller or configure GitHub
access. Registering a watch starts or reuses one background poller per registry. It
routes comments to online sessions, resumes stopped OpenShell Codex and Claude conversations when their
identity is saved, and publishes queued replies. Unrecoverable sessions keep comments pending. Use `paddock github start`, `stop`, and `status --json` to manage
it. Agent exits leave the shared poller running. After reboot, start it again.

### Repository names

Register an optional alias once, then use it from any directory:

```sh
paddock repo add project ~/code/example/project.git
cd ~
paddock run "Implement feature X" --repo project --agent claude
paddock repo list
paddock repo list --json
```

Aliases map names to canonical absolute paths in the global `repos.json` file and support both ordinary checkouts and bare repositories. An alias takes precedence over a same-named relative directory; use `--repo ./project` to explicitly select a path. Without `--repo`, Paddock still discovers the current repository normally.

`paddock repo remove project` removes only the alias; it leaves the repository, tasks, and worktrees intact. Remove an alias before mapping the same name to a different location. Existing tasks retain their resolved repository path, so changing aliases does not redirect delegated work. Alias writes use the same process lock and atomic file replacement as task records.

### Repository trust

To trust the worktrees Paddock creates for a repository:

```sh
paddock repo trust project
# Stop applying trust to future tasks:
paddock repo untrust project
```

Both commands accept an alias or a repository path. Trust is opt-in and belongs to the canonical Git common directory, so linked worktrees and delegated Codex children share the decision. Removing an alias does not revoke trust; use `untrust` first, or pass the repository path afterward.

For new Codex tasks, Paddock passes the task worktree's `projects.<path>.trust_level` setting through `--config`. This avoids the repeated folder trust prompt and allows Codex to load that project's configuration. It does not edit your global Codex config or add approval/sandbox bypass flags. Claude's folder trust remains managed by Claude. Rex Remote Control is a separate app setting.

Existing sessions keep their launch settings. `untrust` stops Paddock from supplying trust to future tasks; it does not revoke trust you independently saved in Codex.

Trusted host MCP connections are unavailable for agent tasks because tool calls
must execute inside OpenShell. Remove existing grants with `paddock repo untrust
project --mcp NAME` before resuming an affected task.

### Remembering Codex hook decisions

For repositories trusted with `paddock repo trust`, new Codex sessions reuse hook decisions you already saved in Codex. Approve hooks once through **Review hooks** or **Trust all and continue**; subsequent worktrees can reuse those approvals. Paddock reads your Codex `config.toml` at launch and passes matching decisions as invocation settings, without editing that file or adding `--dangerously-bypass-hook-trust`.

Decisions are shared only between existing Git worktrees of the same repository with byte-identical root `.codex/hooks.json` or `.codex/config.toml` files. Saved disabled decisions are carried too. Decisions already saved for the current worktree take precedence. Conflicting sibling decisions, changed source files, or unavailable source worktrees leave review to Codex. Codex still checks the saved hash against the current hook definition, including changes after Paddock prepared the launch.

Global and plugin hooks retain Codex's normal persistence. Hook-script contents are not independently hashed by Paddock; the retained hash is Codex's hash of the hook definition. `CODEX_HOME` is honored and forwarded to the terminal when set. Already-running sessions retain their startup settings.

### Reusing mise trust

For repositories opted in with `paddock repo trust`, Paddock checks mise trust before
starting the agent executable, including mise shims. It can reuse an existing approval
from another registered worktree of the same repository. This covers bare repositories,
where mise cannot inherit trust from a normal main checkout.

Paddock confirms the source approval through `mise trust --show`, compares root mise
config variants, version files, `package.json`, and mise config directories, then calls
`mise trust` for the matching target config. A changed or additional config, symlink,
unavailable source, explicit ignore, custom discovery override, or paranoid mode leaves
approval to mise. Nested project configs outside those directories remain mise-managed.
The check also applies to Claude tasks in a Paddock-trusted repository.

This grants mise's normal directory-level trust for that individual worktree. It does
not trust the parent worktrees directory or disable trust checks globally. Afterward,
future edits inside that already-trusted directory follow mise's normal trust behavior;
Paddock does not add continuous content enforcement or recursively validate commands
referenced by the config. `paddock repo untrust` stops future reuse and does not revoke
approvals already persisted by mise. Paranoid mode remains entirely under mise's control.

`PADDOCK_MISE_BIN` can select a specific mise executable. With no mise installed, launch
is unchanged. Tests against native mise use isolated configuration/state:

```sh
PADDOCK_LIVE_MISE=1 mise exec -- go test ./internal/paddock -run TestLiveMiseTrustReuse -v
```

## Commands

| Command | Behavior |
| --- | --- |
| `paddock new` | Open an interactive task form and display the created Rex session. |
| `paddock rex setup\|remove` | Install or remove the floating task launcher and shortcut in Rex's configuration. |
| `paddock run <prompt\|->` | Persist a task with an authorized identity and profile, create its worktree, launch the agent in OpenShell, and return its ID; `-` reads a multiline prompt from stdin. |
| `paddock tasks` | List all repositories' tasks with recorded process status and current terminal observations. |
| `paddock show <id>` | Show provenance, prompt, task and native turn state, pending access requests, terminal, and exit status. |
| `paddock artifacts <id>` | Report commits, workspace changes, and PRs for a task. |
| `paddock message <id> <text>` | Send a native follow-up with an optional idempotency key. |
| `paddock identity create\|show\|verify` | Create, inspect, and verify a named agent identity. |
| `paddock attach <id>` | Select the terminal in a Rex app, or attach to Screen in the current terminal. |
| `paddock delegate "prompt" --parent <id>` | Create a child with an independent worktree and terminal. |

Task IDs accept unique prefixes. `run`, `delegate`, `tasks`, and `show` accept `--json` for scripting. Prompts are passed as argument values, read from stdin with `-`, or loaded with `--prompt-file <path>`; they are never evaluated as shell code. Use `--` before a prompt that begins with a dash.

`run` and `delegate` accept `--agent codex|claude`,
`--terminal auto|rex|screen`, `--repo <directory>`, `--base <ref>`,
`--identity <name>`, and `--profile investigation|implementation`. For
integration calls, also use `--requester`, `--requester-source`, `--external-id`,
and `--context-file`. Options may precede or follow the prompt.

### Delegation

Use native subagents for bounded independent reviews and checks when available.
Paddock delegation is for work that needs a durable task, a separate worktree/branch,
or a reconnectable terminal. Rex children open as background tabs in their parent's
session, including nested children, so the family shares one sidebar section.
If no family session remains, Paddock creates a new section. Resuming a task reuses
the family section when available. Screen children use separate sessions.
Independent context alone does not require that lifecycle.

```sh
# From your own shell:
paddock delegate --parent <id> --agent claude "Implement the frontend"
```

The host owns delegation. An OpenShell agent receives its task ID but cannot
invoke the host Paddock CLI. Ask the host operator to create a durable child
when one is authorized; use native subagents for short reviews inside the
current task.

Use `paddock message TASK_ID "instruction or result"` to communicate with an existing
Paddock agent. Terminal typing can interfere with the user's input and is not a
fallback for native agent messaging.

Children inherit the parent's repository and terminal backend, unless overridden. Agent selection defaults to Codex and can be set explicitly. Each child starts from the repository's default branch, not uncommitted parent work. To build on committed parent work:

```sh
paddock delegate --parent <id> --agent claude --base paddock/<full-parent-id> "Continue this implementation"
```

No work is automatically merged, committed, pushed, or turned into a PR.

### Rex

The adapter uses commands verified against the installed Rex CLI and live local server:

- `rex whoami --json` discovers the endpoint; Paddock requires a local Unix socket and persists that endpoint.
- `rex new <name> -c <worktree> --shell=none --json -- <command>` creates the terminal.
- `rex window new -s <parent-session> <name> --focus=false ...` opens a delegated task's tab.
- `rex ls --json`, `rex session inspect`, and `rex block call ... process` inspect it.
- `rex -C <client> actions --json` verifies support before `do session.select` reconnects the app.

There is no assumed `rex attach` or `rex resume` command. Reconnecting selects the existing terminal; it does not restart the agent. New Rex panes close when their agent process exits; the task record retains its exit status.

The selected agent owns the terminal's foreground process group. Rex sees `codex` or `claude` as the foreground process for its native agent UI, while Paddock waits in the background and records the agent's exit.

When invoked inside Rex, Paddock uses the app that owns the invoking terminal
pane, even if multiple apps are connected. To select another app explicitly:

```sh
rex client ls --json
paddock attach <id> --client <client-id>
```

`paddock run --client <id> "prompt"` targets a particular app for automatic display;
`REX_CLIENT` also applies. Selection prefers `--client`, then `REX_CLIENT`, then
the invoking pane's connected app. If the invoking pane cannot be resolved,
Paddock selects the sole connected app or asks for `--client` when ambiguous.

The selected app must allow **Remote Control** in **Settings → Rex Server**. When disabled, Paddock reports the rejection and an attach command; the created task continues independently. A display failure does not turn a successful launch into a failed command. Creating sessions and inspecting processes do not require this app setting. If no app is connected, open Rex before attaching.

Bare `paddock attach` opens a host shell in a new Rex tab. From Claude Code or
Codex's shell-command mode, use `! paddock attach`. Supplying a task ID retains
the existing behavior of reconnecting to that agent.

OpenShell tasks have a small task-scoped `paddock attach` shortcut. It asks the
host's shared Paddock process to open a shell tab on the Rex app viewing that
task. The shell runs on the host Mac directly in the sandbox's live worktree,
using OrbStack's existing container file share, with `PADDOCK_TASK_ID` set.
Inspect files with macOS tools immediately; agent updates appear in place:

The inspection shell ignores sandbox project configuration during host mise
activation, so browsing artifacts does not require trusting that configuration.

```sh
open doc/features/mira-architecture-convergence.md
```

Host inspection of OpenShell files currently requires a local OrbStack Docker
runtime with its container share available under `~/OrbStack/docker/containers`.
Paddock resolves the workload container by its sandbox ID, name, workspace, and
role labels. If that live view is unavailable, it reports the error before
opening a tab. Set `PADDOCK_DOCKER_BIN` if Docker is not on PATH.

The shortcut is installed for new tasks and active tasks by the shared host
process. `paddock shell-setup <id>` installs it explicitly. After an upgrade,
restart an older shared process with `paddock github stop` and
`paddock github start`. The sandbox shortcut supports only opening its own host
shell; no host command, path, or client selection is accepted from the sandbox.

Paddock looks for `rex` on PATH, then the standard Rex/Rex Beta app-bundle locations on macOS. Set `PADDOCK_REX_BIN` for another location. This adapter uses Rex's CLI, not its private sockets directly. See the [Rex documentation](https://www.superlogical.com/rex/docs) and `rex __internal skill` for the installed version's contract.

### GNU Screen fallback

```sh
paddock run "Implement feature X" --agent claude --terminal screen
paddock attach <id>
```

Screen sessions run detached from the launching CLI. Detach with **Ctrl-A, then D**; the agent keeps running. Paddock ignores personal `.screenrc` commands when creating sessions. A Screen session ends when the agent exits; task records and worktrees remain, but that terminal can no longer be attached. New Rex panes also close on process exit; older retained panes are closed when resuming.

## GitHub coordination

New comments from approved authors on a watched PR go directly to its task session,
without mentions or command prefixes. The poller adds 👀 after confirmed delivery;
the agent interprets the comment and posts results through a developer or team GitHub
App identity. Named profiles map
repositories or owners to personal/team Apps, with automatic installation-token refresh
and protected local key storage. See the [setup and operating guide](github.md)
for authentication, routing, explicit watches, and restart behavior.

## Files and durability

By default, one global registry lives at `${XDG_STATE_HOME:-~/.local/state}/paddock`. Set `PADDOCK_HOME` to an absolute path to use a separate registry. It must be outside the source repository checkout.

```text
paddock/
  .lock
  repos.json          # optional repository aliases and explicit Codex trust
  tasks/
    <id>.json
  worktrees/
    <id>/
```

Each versioned JSON record contains immutable task identity, parent ID, prompt, repository/worktree information, and a nested session record. Parent-child relationships are derived from `parent_id`; no separate graph index is needed. `tasks` and child lookup scan the records.

Every Paddock state operation uses the same short-lived OS file lock. A mutation reads the latest record while holding the lock, writes and syncs a temporary file, atomically replaces the record, and syncs its directory on Unix. Process death releases the lock; the `.lock` file itself is intentionally retained. Incomplete temporary files are ignored. Locks are never held across Git operations, terminal requests, or agent execution. Malformed records fail visibly rather than being silently dropped or overwritten.

Use a local filesystem. Concurrent manual edits, network-filesystem locking semantics, and synchronization services are outside v0's contract. Read the JSON directly freely; make manual edits only while Paddock processes are stopped. There is no SQLite database, lifecycle daemon, background indexer, or migration from the discarded TypeScript prototype.

### Lifecycle and recovery

A task moves through `preparing → starting → running → stopped/failed`. `stopped` means the agent process exited with code zero; process exit does not mark the work complete. Older records may still say `completed`. Interactive agents can remain `running` while awaiting input or after answering a prompt. Paddock does not infer model activity from terminal text.

The terminal runs `paddock __run <id>`, an internal supervisor that launches the agent with inherited terminal I/O and records its exit. Closing the original `paddock run` process does not stop it. Ordinary agent approval, authentication, and sandbox settings are preserved; Paddock does not bypass them.

`tasks` and `show` reconcile terminal observations. A confirmed missing/exited terminal without a recorded agent exit becomes `interrupted`. An unreachable server is reported as `unknown` and does not erase the last known process status. Terminal metadata is stored before returning from launch; if a Rex launch reply is lost, the deterministic session or tab name allows later recovery. A timed-out launch is never blindly retried.

Git and a terminal server cannot participate in a filesystem transaction. Failed launches retain their task record, error, branch, and any created worktree. If the launching CLI dies during preparation, the record can remain `preparing`; inspect the worktree and terminal before starting a replacement task. Use `paddock resume TASK` to resume an existing native conversation. Uncertain launches are not automatically retried.

Tasks survive Paddock restarts. Terminals survive CLI disconnects while their terminal server and host remain running; host reboot survival is not promised. The worktree belongs to the task, independent of whether its agent process is still alive.

`paddock gc` previews workspace cleanup; `paddock gc --apply` rechecks and removes only
expired, safe worktrees. The default retention period is seven days after activity.
Configure it with `paddock lifecycle configure --worktree-ttl 168h`. Dirty/unpushed
work, active processes, open PRs, pending work, and unknown check results prevent
removal. Conversations and branch refs are retained. See [lifecycle and recovery](lifecycle.md)
for artifact policy, recovery, and the current automatic-shutdown limitation.

### Git behavior

Repository identity uses Git's common directory, including when launched from a linked worktree, bare repository, or subdirectory. Base selection uses locally available refs in this order: `origin/HEAD`, `origin/main`, `origin/master`, configured `init.defaultBranch`, `main`, `master`. For other layouts, specify `--base`. Paddock does not fetch automatically: run `git fetch` yourself when you want fresher remote refs. The resolved commit is recorded before worktree creation.

Paddock creates branch `paddock/<id>` and checks it out in the task's worktree. It does not switch, stash, reset, or edit the original checkout or its index. OpenShell tasks use a private Git database inside their retained sandbox. Post-checkout hooks are disabled for worktree creation.

Worktrees stay warm until explicit `paddock gc --apply` passes the retention and safety checks. Task JSON records refer to native conversation history; the JSON alone is not a conversation or filesystem backup.

## Architecture and language choice

The project uses Go, pinned through mise. `cmd/paddock` handles CLI parsing/output; `internal/paddock` contains the task model, file store, Git operations, terminal adapters, and supervisor. Dependencies are `golang.org/x/sys` for OS integration and `github.com/pelletier/go-toml/v2` for reading Codex's saved hook decisions. No cgo is needed.

Go fits the current control plane: small CLI, JSON state, subprocess coordination, and straightforward binary distribution. Rust is also suitable, especially if Paddock eventually implements isolation primitives itself. Zig offers strong C integration and cross-compilation, but those strengths do not offset its additional implementation work for this milestone. The earlier SQLite preference was replaced with files and serialized updates.

OpenShell owns sandbox isolation and credential substitution. Paddock owns task identity, native conversation references, terminal attachment, conservative collection, and GitHub routing. OrbStack can supply the Docker VM underneath the OpenShell gateway.

## Development and tests

```sh
mise run build
mise run check
```

Tests cover restart persistence, parent-child lookup, concurrent writers,
atomic state, repository resolution, identity ownership and routing, immutable
provenance, credential boundaries, terminal observation, lifecycle recovery,
and profile authorization. Legacy host-agent terminal tests are skipped while
host LLM execution is unavailable. Live OpenShell and GitHub App checks are
recorded in the [identity acceptance notes](identity-acceptance.md).

For development and wrappers, `PADDOCK_CODEX_BIN`, `PADDOCK_CLAUDE_BIN`, `PADDOCK_REX_BIN`, and `PADDOCK_SCREEN_BIN` override executable locations. Values are executable paths, not shell command strings.

Cross-compilation example:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 mise exec -- go build -o dist/paddock-linux-amd64 ./cmd/paddock
```

Native macOS operation is tested. Linux and Windows compilation is checked separately from runtime behavior; native Windows terminal support remains out of scope for this slice.

## License

Copyright 2026 Paddock contributors. Licensed under the [Apache License, Version 2.0](../LICENSE).

### Integration dispatch

Use `paddock run --external-id`, `paddock message --external-id`, and
`paddock artifacts --json` for deterministic integration dispatch. See
[integration CLI](integrations.md). Model-backed orchestration is unavailable
until its planner runs in OpenShell.
