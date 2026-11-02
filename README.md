# Paddock

A local control plane for coding agents. Give a task its own Git worktree and persistent terminal, inspect it later, and let an agent delegate a child task to Codex or Claude Code.

Paddock v0 runs on the host. It uses your installed agent CLIs and their existing login/subscription. It makes no direct model API calls and requires no hosted service.

## Setup

Requirements: Git, a logged-in `codex` or `claude`, and either a running local Rex server or GNU Screen. macOS is the tested development platform; the terminal adapters also target Unix environments. Native Windows terminal execution is not implemented (use WSL). Building a Windows binary alone does not provide Windows terminal support.

Development tooling is pinned with [mise](https://mise.jdx.dev/):

```sh
mise trust
mise install
mise run build
mise run check
install -m 755 bin/paddock ~/.local/bin/paddock
```

Ensure `~/.local/bin` is on your PATH. Go is only needed to build; the resulting executable has no language runtime or database dependency. Keep the executable at a stable path while tasks are running: their delegation instructions refer to it.

```sh
cd /path/to/any/git/repository
paddock run "Implement feature X" --agent codex
paddock tasks
paddock show <task-id>
paddock attach <task-id>
```

The directory must have an existing Git commit. No repository registration is needed. Default agent: `codex`. Default terminal: use a reachable local Rex server, otherwise GNU Screen. Use `--terminal rex` or `--terminal screen` to choose explicitly. An explicit backend failure never silently launches another session elsewhere.

Bare repositories are supported too. Point at the bare repository itself; Paddock creates a separate task worktree from it:

```sh
paddock run "Implement feature X" --repo ~/code/example/project.git
```

A plain directory named `project` next to `project.git` is not automatically a checkout. Paddock follows Git's repository discovery and does not guess sibling repository names. An existing linked worktree is also a valid starting directory.

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

### Codex repository trust

To trust the worktrees Paddock creates for a repository:

```sh
paddock repo trust project
# Stop applying trust to future tasks:
paddock repo untrust project
```

Both commands accept an alias or a repository path. Trust is opt-in and belongs to the canonical Git common directory, so linked worktrees and delegated Codex children share the decision. Removing an alias does not revoke trust; use `untrust` first, or pass the repository path afterward.

For new Codex tasks, Paddock passes the task worktree's `projects.<path>.trust_level` setting through `--config`. This avoids the repeated folder trust prompt and allows Codex to load that project's configuration. It does not edit your global Codex config or add approval/sandbox bypass flags. Claude's folder trust remains managed by Claude. Rex Remote Control is a separate app setting.

Existing sessions keep their launch settings. `untrust` stops Paddock from supplying trust to future tasks; it does not revoke trust you independently saved in Codex.

### Remembering Codex hook decisions

For repositories trusted with `paddock repo trust`, new Codex sessions reuse hook decisions you already saved in Codex. Approve hooks once through **Review hooks** or **Trust all and continue**; subsequent worktrees can reuse those approvals. Paddock reads your Codex `config.toml` at launch and passes matching decisions as invocation settings, without editing that file or adding `--dangerously-bypass-hook-trust`.

Decisions are shared only between existing Git worktrees of the same repository with byte-identical root `.codex/hooks.json` or `.codex/config.toml` files. Saved disabled decisions are carried too. Decisions already saved for the current worktree take precedence. Conflicting sibling decisions, changed source files, or unavailable source worktrees leave review to Codex. Codex still checks the saved hash against the current hook definition, including changes after Paddock prepared the launch.

Global and plugin hooks retain Codex's normal persistence. Hook-script contents are not independently hashed by Paddock; the retained hash is Codex's hash of the hook definition. `CODEX_HOME` is honored and forwarded to the terminal when set. Already-running sessions retain their startup settings.

## Commands

| Command | Behavior |
| --- | --- |
| `paddock run "prompt"` | Persist a task, create its worktree, launch the agent, and return its ID. |
| `paddock tasks` | List all repositories' tasks with recorded process status and current terminal observations. |
| `paddock show <id>` | Show the prompt, parent, children, repository, base commit, branch, worktree, terminal, and exit status. |
| `paddock attach <id>` | Select the terminal in a Rex app, or attach to Screen in the current terminal. |
| `paddock delegate "prompt" --parent <id>` | Create a child with an independent worktree and terminal. |

Task IDs accept unique prefixes. `run`, `delegate`, `tasks`, and `show` accept `--json` for scripting. Prompts are passed as argument values, never evaluated as shell code. Use `--` before a prompt that begins with a dash.

`run` and `delegate` accept `--agent codex|claude`, `--terminal auto|rex|screen`, `--repo <directory>`, and `--base <ref>`. Options may precede or follow the prompt.

### Delegation

```sh
# From your own shell:
paddock delegate --parent <id> --agent claude "Implement the frontend"

# Inside an agent launched by Paddock:
paddock delegate --agent claude "Implement the frontend"
```

The supervisor exports `PADDOCK_TASK_ID`, `PADDOCK_PARENT_TASK_ID`, and `PADDOCK_HOME`, and adds Paddock's executable directory to PATH. The agent prompt includes an absolute delegation command with an explicit parent ID and registry path, so delegation can also work when an agent filters environment variables. The agent's ordinary tool approvals still apply.

Children inherit the parent's repository and terminal backend, unless overridden. Agent selection defaults to Codex and can be set explicitly. Each child starts from the repository's default branch, not uncommitted parent work. To build on committed parent work:

```sh
paddock delegate --parent <id> --agent claude --base paddock/<full-parent-id> "Continue this implementation"
```

No work is automatically merged, committed, pushed, or turned into a PR.

### Rex

The adapter uses commands verified against the installed Rex CLI and live local server:

- `rex whoami --json` discovers the endpoint; Paddock requires a local Unix socket and persists that endpoint.
- `rex new <name> -c <worktree> --keep-open --shell=none --json -- <command>` creates the terminal.
- `rex ls --json`, `rex session inspect`, and `rex block call ... process` inspect it.
- `rex -C <client> actions --json` verifies support before `do session.select` reconnects the app.

There is no assumed `rex attach` or `rex resume` command. Reconnecting selects the existing terminal; it does not restart the agent. `--keep-open` preserves a Rex pane after exit for inspection.

The selected agent owns the terminal's foreground process group. Rex sees `codex` or `claude` as the foreground process for its native agent UI, while Paddock waits in the background and records the agent's exit.

If multiple Rex apps are connected, select one:

```sh
rex client ls --json
paddock attach <id> --client <client-id>
```

The selected app must allow **Remote Control** in **Settings → Rex Server**. When disabled, Paddock reports the rejection and the session ID; you can open that session manually. Creating sessions and inspecting processes do not require this app setting. If no app is connected, open Rex before attaching.

Paddock looks for `rex` on PATH, then the standard Rex/Rex Beta app-bundle locations on macOS. Set `PADDOCK_REX_BIN` for another location. This adapter uses Rex's CLI, not its private sockets directly. See the [Rex documentation](https://www.superlogical.com/rex/docs) and `rex __internal skill` for the installed version's contract.

### GNU Screen fallback

```sh
paddock run "Implement feature X" --agent claude --terminal screen
paddock attach <id>
```

Screen sessions run detached from the launching CLI. Detach with **Ctrl-A, then D**; the agent keeps running. Paddock ignores personal `.screenrc` commands when creating sessions. A Screen session ends when the agent exits; task records and worktrees remain, but that terminal can no longer be attached. Rex retains exited panes instead.

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

Use a local filesystem. Concurrent manual edits, network-filesystem locking semantics, and synchronization services are outside v0's contract. Read the JSON directly freely; make manual edits only while Paddock processes are stopped. There is no SQLite database, daemon, background indexer, or migration from the discarded TypeScript prototype.

### Lifecycle and recovery

A task moves through `preparing → starting → running → completed/failed`. `completed` means the agent process exited with code zero; it does not assert that the requested code change is correct. Interactive agents can remain `running` while awaiting input or after answering a prompt. Paddock does not infer model activity from terminal text.

The terminal runs `paddock __run <id>`, an internal supervisor that launches the agent with inherited terminal I/O and records its exit. Closing the original `paddock run` process does not stop it. Ordinary agent approval, authentication, and sandbox settings are preserved; Paddock does not bypass them.

`tasks` and `show` reconcile terminal observations. A confirmed missing/exited terminal without a recorded agent exit becomes `interrupted`. An unreachable server is reported as `unknown` and does not erase the last known process status. Terminal metadata is stored before returning from launch; if a Rex launch reply is lost, the deterministic session name allows later recovery. A timed-out launch is never blindly retried.

Git and a terminal server cannot participate in a filesystem transaction. Failed launches retain their task record, error, branch, and any created worktree. If the launching CLI dies during preparation, the record can remain `preparing`; inspect the worktree and terminal before starting a replacement task. No automatic retry, worktree deletion, or agent-conversation resume is implemented in v0.

Tasks survive Paddock restarts. Terminals survive CLI disconnects while their terminal server and host remain running; host reboot survival is not promised. The worktree belongs to the task, independent of whether its agent process is still alive.

### Git behavior

Repository identity uses Git's common directory, including when launched from a linked worktree, bare repository, or subdirectory. Base selection uses locally available refs in this order: `origin/HEAD`, `origin/main`, `origin/master`, configured `init.defaultBranch`, `main`, `master`. For other layouts, specify `--base`. Paddock does not fetch automatically: run `git fetch` yourself when you want fresher remote refs. The resolved commit is recorded before worktree creation.

Paddock creates branch `paddock/<id>` and checks it out in the task's worktree. It does not switch, stash, reset, or edit the original checkout or its index. Git necessarily adds worktree metadata and a new branch to the shared repository. Post-checkout hooks are disabled for worktree creation. This is isolation of working directories, not a security sandbox.

Worktrees and records are kept until you remove them deliberately. Once an agent has stopped and you have preserved its changes, use Git's normal `worktree remove` and branch-management commands. Keeping its JSON record preserves the history.

## Architecture and language choice

The project uses Go, pinned through mise. `cmd/paddock` handles CLI parsing/output; `internal/paddock` contains the task model, file store, Git operations, terminal adapters, and supervisor. Dependencies are `golang.org/x/sys` for OS integration and `github.com/pelletier/go-toml/v2` for reading Codex's saved hook decisions. No cgo is needed.

Go fits the current control plane: small CLI, JSON state, subprocess coordination, and straightforward binary distribution. Rust is also suitable, especially if Paddock eventually implements isolation primitives itself. Zig offers strong C integration and cross-compilation, but those strengths do not offset its additional implementation work for this milestone. The earlier SQLite preference was replaced with files and serialized updates.

Future OpenShell or OrbStack VM execution can change the launch boundary (executable, arguments, working directory, environment) without changing task ownership. Neither integration is implemented here; there is no speculative sandbox framework or intelligent model router.

## Development and tests

```sh
mise run build
mise run check
# Real terminal integration; requires access to the terminal server:
PADDOCK_LIVE_TERMINAL=screen mise exec -- go test ./internal/paddock -run TestLiveTerminalLifecycle -v -count=1
PADDOCK_LIVE_TERMINAL=rex mise exec -- go test ./internal/paddock -run TestLiveTerminalLifecycle -v -count=1
# Native Codex hook trust checks; isolated config, no model calls or hook execution:
PADDOCK_LIVE_CODEX=1 mise exec -- go test ./internal/paddock -run TestLiveCodexHookTrust -v -count=1
```

Tests cover restart persistence, parent-child lookup, concurrent writers in separate processes, atomic readers, lock recovery after process death, corrupt records, repository/default-ref resolution, unchanged source checkouts, linked worktrees, repository trust and revocation, supervisor exits, literal prompt handling, terminal observation failures, and Rex metadata recovery. Live tests run real Paddock subprocesses in real terminals with deterministic fixture agents; the parent invokes the CLI to delegate its child. Rex tests also verify the agent owns the foreground process group. These tests do not consume model subscriptions.

For development and wrappers, `PADDOCK_CODEX_BIN`, `PADDOCK_CLAUDE_BIN`, `PADDOCK_REX_BIN`, and `PADDOCK_SCREEN_BIN` override executable locations. Values are executable paths, not shell command strings.

Cross-compilation example:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 mise exec -- go build -o dist/paddock-linux-amd64 ./cmd/paddock
```

Native macOS operation is tested. Linux and Windows compilation is checked separately from runtime behavior; native Windows terminal support remains out of scope for this slice.

## License

Copyright 2026 Paddock contributors. Licensed under the [Apache License, Version 2.0](LICENSE).
