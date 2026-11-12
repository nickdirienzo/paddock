# OpenShell runtime acceptance

## Runtime integration — 2026-11-03

The implementation now defaults new tasks to OpenShell; legacy tasks stay on
their existing runtime and `--runtime host` is explicit. The installed production
CLI/configuration was not changed during these isolated tests.

Verified with OpenShell 0.1.6, Codex 0.159.2, OrbStack, and a dedicated temporary
Paddock registry against `nickdirienzo/paddock`:

- A native Codex turn used ordinary `gh api installation/repositories` and saw
  only `nickdirienzo/paddock`, then wrote `runtime-smoke-marker` in its private
  checkout. No PRs, comments, or pushes were made in this integration test.
- A stable GitHub provider placeholder survived token replacement and explicit
  revocation of the old token in the **same running process**. This resolves the
  static-revision failure described in the earlier experiment below.
- Paddock delivered a native queued turn to the existing daemon-owned conversation.
  Rex identified the foreground executable as `codex`.
- After confirmed native idle and explicit agent exit, Paddock stopped the sandbox.
  Cold resume restored the same conversation ID and visible history. A subsequent
  queued model turn read the original `warm-workspace` marker successfully.
- A pending native approval was rejected by the idle guard. Terminal inactivity
  was never used as a completion signal. Idle ephemeral native helper threads
  were checked without calling their unsupported queue interface.
- Aging only the disposable task beyond seven days and running `gc --apply`
  retained its checkout with `uncommitted or untracked changes`. Maintenance
  compute returned to `Stopped` afterward.
- The Go race suite and vet passed. Linux and Windows compilation passed;
  that does not establish native Windows terminal support.

Fixes found by the live tests: nested Bubblewrap cannot create user namespaces
inside this runtime; remote Codex resume rejects CLI permission overrides and
needs server-side thread loading to avoid its read-only fallback; native daemon
model calls need the daemon's managed binary path in the provider profile.

All disposable agent sessions were closed after confirming exit, their sandbox
compute was stopped, and the isolated poller was stopped. Warm files and native
histories remain in retained test sandboxes. Production sessions were untouched.

Not yet qualified: an hour-long token expiration soak, gateway restart recovery,
and Mirage's full dependency/build environment. The Codex test used
an explicitly selected access token without copying or rotating the host refresh
token. Production setup requires a dedicated model login owned by the gateway.

### Claude Code follow-up

Claude Code 2.1.286 passed the live test against `nickdirienzo/paddock`:

- Native launch through Paddock and Rex foreground recognition as `claude`.
- A real model turn ran ordinary `gh api installation/repositories` and received
  only the test repository, then wrote an untracked workspace marker.
- The workload received an opaque OAuth reference. The user's explicitly approved
  access token stayed in the gateway with an expiry; its refresh token stayed on
  the host. No PRs, comments, or pushes were made.
- A native `end_turn` record confirmed completion before explicit interactive
  exit. Paddock stopped compute while preserving the native transcript and files.
- `paddock resume` retained the exact conversation UUID. A test-only resume prompt
  exercised the native CLI prompt argument: the model recalled **purple heron**
  from the first turn and read **warm-claude-workspace** from the original marker.

The test found and fixed two image/policy setup gaps: Claude's unauthenticated
startup check requires read access to `platform.claude.com`, and its first-run
wizard must be completed in the preconfigured image. Workspace trust and tool
approvals remained enabled. PR-comment delivery was still Codex-only at this stage.
After the test, both Claude sandbox fixtures were stopped, their model and GitHub
credential providers were detached and deleted, and the isolated poller and
disposable Rex sessions were closed. Native history and warm files were retained.

### Claude inbox and dedicated OAuth renewal

A second, explicitly authorized login was created in a disposable login container,
separate from the host Keychain. `configure-claude.py` imported it into the gateway
and successfully refreshed it; a second gateway rotation also succeeded. A running
Claude process completed another model turn after rotation without restarting or
receiving a new environment. Gateway refresh status showed the next scheduled
renewal and bounded expiry. The host login's refresh token was never transferred.

The final image uses Claude's native Unix inbox. Consecutive `paddock message`
calls were confirmed by exact persisted native enqueue records, including a
follow-up queued during a foreground `sleep 20`. After explicit exit, the message
path resumed the same conversation UUID and generation advanced to 2. A startup
race returned safely retryable `offline` before any bytes were sent; retry reached
the resumed inbox. The native session responded with
its remembered phrase **copper finch** and read the original warm marker. A socket
write or diagnostic log alone is deliberately insufficient: the first version of
the adapter exposed an intermittent acknowledgment failure when Claude redacted its
debug log. The final adapter needs no debug logging.

Linux protocol tests cover accepted, held/refused, missing, and stale-generation
inboxes. Held/refused or uncertain deliveries do not receive a success receipt and
are never blindly retried. The ordinary GitHub poller's existing receipt/deduplication
path is reused; no new poller, model planner, or terminal typing is involved. Host
Claude message routing remains unsupported. Claude keeps native tool
approvals; untrusted repositories still receive the workspace trust prompt.

The remainder records the earlier standalone experiment and its findings at that
time; it is not a description of the current implementation.

## Standalone experiment — 2026-11-02

Executed on 2026-11-02 against `nickdirienzo/paddock`. This is an acceptance
experiment, not a Paddock runtime implementation or a change to the default runtime.

## Environment

- Apple Silicon macOS; OrbStack Docker Engine 29.8.2.
- OpenShell CLI, gateway, and prover 0.1.6, downloaded from the tagged upstream
  release and verified against its published SHA-256 manifests.
- Separate local gateway on port 17679 with TLS and mTLS, isolated configuration,
  state, and certificates. Existing Paddock/Rex sessions were untouched.
- Docker compute driver, bridge-networked supervisor, workload network disabled,
  non-root UID 1000, and `landlock.compatibility: hard_requirement`.
- Dedicated image with Go 1.27.2, Git, and gh; no host home or credential mounts.
- Personal Paddock GitHub App, with an installation token explicitly restricted
  to `nickdirienzo/paddock`. Permissions: Contents, Pull requests, and Issues write;
  Metadata read. The operator enabled Contents write during the experiment.

The App key stayed on the host. The token was passed to OpenShell through the
provider CLI's environment lookup, never a command argument or sandbox environment.
The sandbox received an opaque credential placeholder. No private key, token, or
certificate is included in these fixtures or this report.

## Verified

1. Gateway registration reported authenticated mTLS access. Sandbox readiness and
   configuration admission both succeeded on OrbStack.
2. Sandboxed `gh api installation/repositories` returned only
   `nickdirienzo/paddock`.
3. The sandbox was non-root and had no host `/Users/nickdirienzo`, personal
   `gh/hosts.yml`, or `SSH_AUTH_SOCK`. A hash comparison confirmed its `GH_TOKEN`
   value differed from the real installation token; it was not a `ghs_` token.
4. Access to `mirage-security/mirage` from that sandbox was denied.
5. Ordinary `gh auth setup-git` and `git clone` worked through the provider.
6. `go test ./...` passed for both Paddock packages inside the sandbox, including
   subprocess and local HTTP test fixtures. Go module downloads used explicit
   read-only policy for `proxy.golang.org`, `sum.golang.org`, and
   `storage.googleapis.com`.
7. Ordinary `git push` created a disposable branch, and ordinary `gh pr create`
   opened [draft PR #8](https://github.com/nickdirienzo/paddock/pull/8).
   GitHub reported its author as `paddock-eng[bot]`.
8. `gh pr comment` posted an [acceptance reply](https://github.com/nickdirienzo/paddock/pull/8#issuecomment-6590480925)
   as the App. `gh api` added an `eyes` reaction, also as `paddock-eng[bot]`.
9. The sandbox closed the test PR without merging and deleted its remote branch.
   Sandbox deletion completed and the temporary provider was removed.

These were deterministic command tests. No model turn or native Codex/Rex agent
session was launched. The test used a sandbox-local clone, not a mounted host Git
worktree.

## Credential rotation finding

Static provider updates are insufficient for long-running agent sessions:

1. Start a long-lived sandbox process with the current provider environment and
   verify that a child `gh` command succeeds.
2. Mint a replacement repository-scoped App token on the host and run
   `openshell provider update ... --credential GH_TOKEN --wait`.
3. Revoke the previous test token using GitHub's installation-token endpoint.
4. Run `gh` again as a child of the original process: it fails with HTTP 401.
5. Launch a fresh `openshell sandbox exec ... -- gh ...`: it succeeds.

Updating the provider does not update the environment inherited by an existing
agent and its children. An initial test without revocation appeared to pass because
the old token remained valid; explicit revocation exposed the stale credential.
This was a rotation/revocation test, not an hour-long expiration soak.

OpenShell documents this distinction for static credential revisions in its
[provider guide](https://github.com/NVIDIA/OpenShell/blob/v0.1.6/docs/how-it-works/providers/overview.mdx).
Paddock needs a refresh-aware provider integration or credential broker that works
for existing agent processes before enabling this runtime by default. Restarting
the agent every time an App token expires is not an acceptable session lifecycle.

## Reusable fixtures

See [`experiments/openshell`](../experiments/openshell):

- `Dockerfile`: dedicated test image with a pinned Go base image digest.
- `github-profile.json`: provider profile for this repository, consumed as YAML/JSON
  by OpenShell. Only Git and gh can use its GitHub endpoints. REST writes are scoped
  to the test repository; GraphQL access is additionally constrained by the
  repository-scoped GitHub token.
- `policy.json`: explicit sandbox filesystem and Go dependency network policy.

Build the image and import the profile on a separately configured gateway:

```sh
docker build -t paddock-openshell-test:0.1.6 experiments/openshell
openshell profile lint --file experiments/openshell/github-profile.json
openshell profile import --file experiments/openshell/github-profile.json
```

Create a provider using a repository-scoped installation token supplied by the
host credential manager. Use bare `--credential GH_TOKEN` to read it from the
provider CLI's environment; do not use automatic discovery of a personal gh login.

```sh
openshell provider create --name paddock-app --type paddock-github \
  --credential GH_TOKEN
openshell sandbox create --name paddock-probe \
  --from paddock-openshell-test:0.1.6 \
  --policy experiments/openshell/policy.json \
  --provider paddock-app --no-auto-providers --detach -- /bin/sleep infinity
openshell sandbox exec paddock-probe -- gh api installation/repositories \
  --jq '.repositories[].full_name'
```

The installed release rejected a 21-character sandbox name with a maximum of 19;
the shorter name above worked. Its exec environment did not retain the image's Go
PATH entry. Supply it explicitly for build/test commands:

```sh
openshell sandbox exec paddock-probe --workdir /sandbox/paddock \
  --env PATH=/usr/local/go/bin:/usr/bin:/bin \
  --env GOCACHE=/sandbox/go-cache --env GOPATH=/sandbox/go-path -- go test ./...
```

## Remaining integration work

- Refresh credentials for existing processes and test actual token expiration.
- Connect Paddock task identity, checkout storage, launch, reconnect, and cleanup
  to the sandbox lifecycle.
- Verify native Codex/Claude operation and Rex agent recognition.
- Deliver watched PR messages into the sandbox's existing agent session.
- Define repository permissions for real workflows, including workflow-file writes
  if needed; this experiment did not change GitHub Actions files.

Host execution remains the default. Paddock's source and global tool configuration
were not changed to depend on OpenShell by this experiment.
