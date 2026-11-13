# OpenShell runtime

All new agent tasks run in OpenShell. Explicit `--runtime host` and saved host
defaults are rejected. Existing host tasks remain inspectable but cannot resume.
Missing sandbox setup fails with setup instructions. `paddock runtime default`
prints the current choice and accepts only `openshell`.

## Setup

Qualify OpenShell **0.1.6** (CLI, gateway, and runtime images together) on a local
Docker/OrbStack gateway with TLS and authenticated access. Install it using your
preferred system tooling, including mise; Paddock does not install or start a
privileged gateway automatically. The gateway should be a persistent user service,
with state and certificates outside temporary directories. The isolated acceptance
gateway is deliberately separate from this production setup.

Build the agent image, then pin its immutable image ID:

```sh
docker build -t paddock-agent:0.1.6 runtime/openshell
docker image inspect paddock-agent:0.1.6 --format '{{.Id}}'
```

The image contains pinned Codex and Claude versions, Git, gh, Go, and a small
launcher. `paddock runtime configure --image` requires a digest reference or a local
`sha256:` image ID. The supplied filesystem/network policy is a starting point for
Go projects. Other repositories may need their own tooling and explicit
dependency endpoints. Configure RAM with room for both the build and
agent; a 16 GB build needs more than a 16 GB total sandbox budget, and the OrbStack
VM needs enough memory too.

For GitHub work, configure an App identity and repository route first. The App
needs Contents, Pull requests, and Issues write permissions. Each App-backed
task pins the mapped principal, gets a separate provider, and mints tokens
restricted to exactly its repository and those permissions. An unbacked logical
identity can run local-only tasks without a GitHub origin or token. Workflow-file
changes require a separate permissions decision; this runtime does not request
Actions/workflows administration.

Model authentication is separate from the GitHub identity. Paddock accepts an
explicit OpenShell provider for each agent; it does not discover personal model
credentials or copy the host home into a sandbox.

For Codex ChatGPT authentication, create a **dedicated login** in a separate
`CODEX_HOME` using `codex login --device-auth`, then import it:

```sh
python3 runtime/openshell/configure-codex.py \
  --gateway paddock \
  --auth-file /path/to/dedicated-codex-home/auth.json
```

OpenShell becomes that login's refresh-token owner. Do not continue using the same
refresh token from a host CLI. The helper passes credentials through environment
lookup, not command arguments. The workload receives a proxied access-token
reference; its refresh token stays in the gateway. The account selector is
non-secret and must remain an actual identifier: Codex compares it locally during
workspace discovery. The helper prints the selector for configuration.

For Claude OAuth, create a **dedicated login** using the pinned Claude CLI in
an isolated container/configuration directory, then import its `.credentials.json`:

```sh
python3 runtime/openshell/configure-claude.py \
  --gateway paddock --auth-file /path/to/dedicated-claude-home/.credentials.json
paddock runtime configure --claude-provider paddock-claude
```

The helper refuses the default host credentials file and performs no Keychain
lookup. Stop using that dedicated login in any CLI after import: the gateway owns
its rotating refresh token. Credentials are passed through environment lookup,
never command arguments or output. The profile sends access credentials only to
`api.anthropic.com`; refresh material stays in the gateway. OpenShell renews five
minutes before expiry, caps the access-token lifetime at one hour, and persists
rotated refresh tokens. An expired or revoked login requires reauthorization;
there is no fallback to personal host credentials. Inspect renewal with
`openshell provider refresh status paddock-claude`.

Claude's native interactive launch/resume and inbox delivery are supported in
OpenShell. The image completes display/login onboarding because authentication
comes from the provider. Claude keeps native tool approvals. For repositories
explicitly trusted with `paddock repo trust`, Paddock records workspace trust in
the task's isolated Claude config before launch; other repositories receive the
native workspace trust prompt.
A separate read-only rule allows Claude's startup connectivity check at
`platform.claude.com`, without credential injection there.

```sh
paddock runtime configure \
  --binary /absolute/path/to/openshell --gateway paddock --workspace default \
  --image sha256:IMAGE_ID \
  --policy "$PWD/runtime/openshell/policy.json" \
  --codex-provider paddock-codex --codex-account-id ACCOUNT_ID \
  --memory 24Gi --allow nickdirienzo
paddock runtime check
paddock run --repo paddock 'Implement the change'
```

For a gateway with isolated configuration directories, also pass `--config-home`,
`--state-home`, and `--tls-dir`. These non-secret references are pinned in the task,
so changing the globally selected OpenShell gateway cannot redirect an existing task.
`runtime check` verifies the qualified CLI and gateway connectivity; task creation
additionally validates the provider profile, credentials, image, and agent binary.

Investigation tasks build a task-specific image from the pinned runtime image
and exactly the selected Git commit and ancestry. The trusted host uses local
Docker to make `/sandbox`, the checkout, and private Git database root-owned, then pins the
resulting image ID before OpenShell starts. The runtime image must already be
present in the local Docker image store used by the gateway. The build context
contains no private key or installation token. A remote gateway needs an image
distribution step before it can run this profile.

The agent runs as the ordinary `sandbox` user. Root ownership denies source
content writes, checkout renames, and metadata changes such as `chmod`. Task
scratch directories under `/sandbox` remain owned by the agent. An additional Landlock
layer denies source entry creation and protects the launcher as well as its
native daemon. Home, `/tmp`, Go cache, and Go module cache remain writable.
Claude's inbox socket lives in
`/home/sandbox/.claude`, which trusted preparation sets to mode `0700` as
required by Claude. The guard also denies socket creation in the
checkout. Rebuild and pin the runtime image after updating Paddock; an older
image cannot launch a guarded Claude investigation. The trusted sandbox launcher
enters the same Landlock rules before starting the agent; its lock and exit
records live in a private home directory. Inspection of the same-UID launcher
cannot restore source write rights. Preparation probes that the worktree rejects
file and socket creation, file-mode changes, and checkout renames while scratch accepts writes;
failure stops launch. An older investigation task
without the current verified guard cannot resume or receive a follow-up; create
a new task. The OpenShell
startup policy still limits the entire sandbox's host and network access.
Guard version 5 leaves the launcher's executable readable to OpenShell's
process identity check. A live App-backed Claude investigation completed a
read-only turn and a native inbox follow-up under the preceding guard; a live
Codex turn under version 4 denied `chmod` and source creation and returned no
code artifacts. Investigation tasks cannot push with their read-only GitHub
token. Keep task-specific images while their sandboxes may resume; the current
worktree collector does not remove the immutable image layer separately.
OpenShell 0.1.6 accepts live network-policy changes, but its filesystem and
process policy is fixed when the workload starts. A credential-free probe with
`process.run_as_user: "1500"` showed that both ordinary exec and the gateway
uploader used UID 1500, so selecting another workload UID does not give the
checkout a separate owner. Image construction supplies the ownership boundary
before the sandbox starts. The probe sandbox was deleted.

## Pasted images

Paste screenshots into a sandboxed Codex or Claude terminal normally. Paddock
recognizes the temporary image paths generated by Rex and Codex, copies the
selected image into that task's `/sandbox/.paddock/attachments` directory, and
inserts the sandbox path before the agent receives the paste. You do not need
a host-shell tab, an upload command, or a filesystem trust exception.

Only explicitly pasted clipboard images in the host user's temporary directory
are transferred. Paddock does not watch the clipboard or share images across
tasks. PNG, JPEG, GIF, and WebP files up to 20 MiB are supported. Transfers use a
private snapshot of a regular file; symlinks and non-image files are refused.
An upload failure appears in the pasted text so you can retry. Attachments are
outside the Git checkout and stay with that task's retained sandbox.

After updating Paddock, exit and resume an existing agent once to activate the
paste transport. New tasks use it automatically. The wrapper preserves terminal
size changes, control keys, native permissions, and the agent's exit status.

## Identity and messages

### Trusted MCP connections

Use `paddock repo trust PROJECT --mcp NAME` to let tasks in that repository
call a selected host Codex MCP connection. Paddock starts a separate host Codex
app-server process for the task. The agent stays in OpenShell, but selected MCP
calls and OAuth refresh execute on the host, outside OpenShell's network policy.
The process does not run a model turn or expose shell tools to the workload.
Credentials stay on the host. Only HTTPS MCP servers from the host Codex config
can be granted; changes to their configuration require a new grant. Revocation
with `paddock repo untrust PROJECT --mcp NAME` takes effect on the next call.
Granting a server exposes its allowed tools and resources to that repository's
agent; Paddock does not inspect the arguments of an allowed tool call.
Paddock records the absolute host Codex executable when you grant an MCP server,
so tasks started by background integrations do not depend on their host `PATH`.
If Codex moves, run the trust command again from a shell that can find it, or
set `PADDOCK_CODEX_BIN` to its new absolute path for that command.

Normal `git` and `gh` commands run inside OpenShell as the mapped App bot. Tokens
are not shared with the installation-wide poller transport. The host retains App
keys. No host home, shared Git directory, SSH agent, Docker socket, or gateway
administrator credential is mounted into workloads.

The existing singleton poller renews task GitHub tokens independently of slow PR
polling. Stable provider placeholders let existing processes use renewed tokens;
provider updates wait for activation and set explicit expiry. If renewal fails,
credentials expire; there is no fallback identity. `paddock show --json` includes
the expiry and last renewal error. After upgrading an older running poller, restart
it with `paddock github stop` followed by `paddock github start`.

With `--allow`, the host poller discovers an open PR by the task's exact repository
and branch and registers the existing watch mechanism. Only the configured account
logins can issue instructions. Use `--allow '*'` to accept any GitHub user or bot that
can comment, including future accounts and external review bots. Paddock's own App
identity and marked replies remain excluded. This sets the policy for newly created
tasks and discovered watches, without widening existing watches.
Without this option, register a watch explicitly
from the host. Sandboxed agents use ordinary `gh` for PRs/replies; the host owns task
coordination. `paddock message TASK 'instructions'` uses the agent's native inbox inside
the sandbox, resuming the saved conversation when needed. Claude support is pinned
to 2.1.286. Paddock verifies the inbox's
process, conversation, and launch generation, then requires an exact persisted
`queue-operation: enqueue` record before acknowledging delivery. A socket write
alone is insufficient. Claude treats these as peer messages: its hold/refuse rules
and approvals still apply. Unconfirmed delivery is not automatically retried.
A newly resumed inbox may briefly return retryable `offline`; the poller retries
these unsent messages. Paddock never types into the TUI or interprets receipt as
turn completion.

## Lifetime and recovery

A private Git database and worktree are seeded from the selected commit and its
ancestry, including a local unpushed base. No repository config or credential helper
is copied. Build state remains in the task's retained container filesystem.

Codex uses `--sandbox danger-full-access --ask-for-approval on-request` **inside
OpenShell**, where the outer runtime enforces isolation. This avoids unsupported
nested Bubblewrap namespaces while keeping native approvals enabled. Hook
approval is not bypassed.
The pinned Codex version needs its saved thread loaded through `thread/resume`
on the native daemon before the remote TUI attaches. The launcher verifies the
outer-runtime policy was accepted; it refuses active/approval states. This avoids
both rejected CLI permission overrides and the remote cold-resume read-only
fallback. Native queue and status calls use the qualified experimental protocol.

The sandbox launcher holds a process lease and writes a generation-specific exit
record after the native process exits. Only that confirmed exit permits the host
supervisor to check native threads and queues and stop the sandbox. Ephemeral
native helper threads must also be idle. Terminal inactivity, model turn completion, or a
lost connection never causes termination. An agent awaiting approval stays alive.
A lost host supervisor cannot create a duplicate agent: resume checks the sandbox
lease. A disconnected, still-running agent requires operator reconciliation.

`paddock resume` restarts the same sandbox and native conversation. `paddock gc`
uses the existing configurable TTL (seven days by default). Sandbox collection
requires confirmed stopped compute, no live terminal or process lease, recoverable
conversation history, no pending work, a clean checkout (including protected ignored
files), live proof of pushed commits, and no open PRs. It briefly starts maintenance
compute for Git/history checks, then stops it even if a check fails. `--apply`
removes only the checkout; the private Git database, branch, and agent home remain.
Resume recreates a collected checkout and explicitly reports that build artifacts
and uncommitted files were not recovered.

Deleting a sandbox through OpenShell/Docker also deletes its retained conversation
and Git state. Paddock never does this as routine GC and refuses to invent a fresh
conversation when that state is missing. This is persistence across process exit,
not a backup or a sandbox snapshotting system.

## Qualification status

The isolated Codex acceptance test exercised repository-scoped `gh`, token
replacement after revoking the old token in a still-running process, native
queued turns, Rex agent recognition, stopped compute, same-conversation cold
resume, warm files, and GC retention of uncommitted files. See the
[acceptance report](openshell-acceptance.md). Claude also passed native launch,
repository-scoped GitHub access, stopped compute, and same-conversation cold
resume with remembered context and warm files. Dedicated Claude OAuth renewal
and continued model access in the same process have also been tested. An actual
expiration soak and a production gateway restart remain unqualified. Deploying
locally requires persistent gateway configuration and a dedicated model login;
the test gateway and model provider are not deployment defaults.
