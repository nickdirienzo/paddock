# OpenShell runtime

New tasks default to OpenShell. `--runtime host` explicitly uses installed host
agents and their existing authentication. Tasks created before this change remain
host tasks; resume never migrates them. Missing sandbox setup fails with setup
instructions, without falling back to host execution or a personal GitHub login.

For an explicit local rollout choice, use `paddock runtime default host` while
preparing the gateway, then `paddock runtime default openshell` to activate it.
`paddock runtime default` prints the current choice. This affects new tasks only;
an explicit `--runtime` flag wins, and resumes retain their saved runtime.

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
Go projects. A Mirage image/policy will also need the monorepo's tooling and
explicit dependency endpoints. Configure RAM with room for both the build and
agent; a 16 GB build needs more than a 16 GB total sandbox budget, and the OrbStack
VM needs enough memory too.

Configure the existing GitHub App identity/routes first. The App needs Contents,
Pull requests, and Issues write permissions. Each task pins the mapped principal,
gets a separate provider, and mints tokens restricted to exactly its repository and
those permissions. Workflow-file changes require a separate permissions decision;
this runtime does not request Actions/workflows administration.

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
comes from the provider; native workspace trust and tool approvals remain enabled.
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

## Identity and messages

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
and branch and registers the existing watch mechanism. Only the configured engineer
logins can issue instructions. Use `--allow '*'` to accept any GitHub user who can
comment, including future users; bot comments remain excluded. This sets the policy
for newly created tasks and discovered watches, without widening existing watches.
Without this option, register a watch explicitly
from the host. Sandboxed agents use ordinary `gh` for PRs/replies; the host owns task
coordination. `paddock message TASK 'instructions'` uses the agent's native inbox inside
the sandbox, resuming the saved conversation when needed. Claude support is pinned
to 2.1.286 and currently limited to OpenShell tasks. Paddock verifies the inbox's
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
nested Bubblewrap namespaces while keeping native approvals enabled. Host tasks
retain their original sandbox/approval settings. Hook approval is not bypassed.
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
