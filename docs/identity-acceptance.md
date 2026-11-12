# Agent identity acceptance notes

## 2026-11-12 live checks

These checks used a temporary Paddock registry copied from the existing
nonsecret configuration. The production registry and GitHub App keys were not
changed.

- `identity verify personal --repo nickdirienzo/paddock` authenticated as
  `paddock-eng[bot]`, confirmed access to the repository, and resolved bot user
  ID `351659889` for Git commit attribution. After scoped profile verification
  was added, the same live command also minted and discarded repository-scoped
  investigation and implementation tokens and reported both profiles verified.
- `identity verify mirage` authenticated as the distinct actor
  `mozziebot[bot]`. `identity github repositories personal` listed 79 accessible
  repositories through the existing installation.
- `runtime check` reached the configured OpenShell gateway.
- An investigation task (`ff0f8998d373edbffbc06c91`) launched inside
  OpenShell against `nickdirienzo/paddock` with the `personal` App. Its task
  record pinned local caller, logical identity, GitHub bot actor, and
  investigation profile. Its artifact report contained no commits, untracked
  files, patch, or PRs.
- The task reported `blocked: true` with pending OpenShell network proposals
  for Codex startup and GitHub endpoints. No proposal was approved. The task
  was stopped, and its task-specific sandbox, GitHub token provider, and
  network profile were deleted. This live run did not confirm a completed
  model turn, push, or PR attribution.
- A second isolated task (`0de025a24c7c16d9553d4aff`) used an unbacked
  `local-check` identity and a local repository with no GitHub origin. It
  reached a running OpenShell sandbox with only the configured model provider.
  Provenance had no GitHub actor or GitHub capability; the sandbox had no
  GitHub provider, token, or repository route. Artifact retrieval reported an
  available workspace and no PR lookup error. The agent still reported pending
  network proposals, so no model turn completed. The exact temporary sandbox
  was stopped and deleted, and the isolated poller was stopped.
- A credential-free OpenShell probe started with `/sandbox` read-only. Agent
  execution could not create a file there. The gateway uploader also failed to
  place a file there, so the normal bootstrap cannot seed a Git worktree under
  that startup policy. The probe sandbox was stopped, deleted, and confirmed
  absent. OpenShell 0.1.6 fixes its filesystem policy at startup; Paddock now
  adds a second Landlock layer to investigation agent descendants *after* the
  trusted host seeds the checkout. See the [OpenShell 0.1.6 policy schema](https://docs.nvidia.com/openshell/v0.1.6/how-it-works/policies/schema).
- The interactive manifest flow registered a fresh `paddock-acceptance` App
  (ID `5732890`) and imported its private key into protected per-identity host
  storage for `acceptance-20261112` in an isolated registry. After human
  installation, Paddock associated installation `181342782` and verified
  `paddock-acceptance[bot]` (bot user ID `355937771`). Repository inventory
  contained only `nickdirienzo/paddock`; `identity verify --repo` successfully
  minted and discarded both the investigation and implementation tokens scoped
  to that repository.
- The isolated acceptance registry has a copy of the nonsecret OpenShell
  configuration. `runtime check` reached the configured gateway from that
  registry before the App-backed launch.
- Implementation task `e96eda336c87bff9060761cf` launched in OpenShell with
  that new App, the implementation profile, and Git author
  `paddock-acceptance[bot]`. It added the requested prompt-boundary test,
  ran `go test ./cmd/paddock`, pushed commit `70979c97a446a5ff4bf63a9aa84e16ea6a2fa245`,
  and opened [draft PR #26](https://github.com/nickdirienzo/paddock/pull/26).
  GitHub reports both the commit author and committer, and the PR creator, as
  `paddock-acceptance[bot]`. Paddock's artifact command returned that exact
  commit, patch, and PR. A follow-up with external ID
  `acceptance-20261112-followup-1` was delivered to the saved Codex thread;
  replaying the same ID returned the same durable receipt. Replaying the
  original `run` command with external ID `acceptance-20261112-pr-1` returned
  the same task ID and branch without starting a second task. Codex's native
  `thread/read` reported two completed turns after the follow-up, confirming
  the saved session handled it.
- OpenShell still listed unused pending network proposals after the task made
  the commit and PR. `show --json` consequently reported `blocked: true` even
  though the requested model turn completed. The pending proposals did not need
  approval for this acceptance task. `show --json` now reads Codex's saved
  native thread and reports `last_turn.state: completed` separately from the
  running terminal and pending access requests.
- A fresh App-backed Codex investigation (`cc2adae2c465087ebe34f162`)
  launched with the read-only guard. Its native tool output shows `touch`
  failed with `Permission denied` under `/sandbox/worktree`, while `git status`
  succeeded. Its native turn completed, and Paddock returned no untracked
  files, commits, patch, or PR. A same-UID sandbox process could not open the
  trusted launcher process's memory. A guarded Claude investigation
  (`4c0f3f80aece9d64240c3d6b`) launched and reached native workspace trust
  before its first model turn. That older task was stopped before trust was
  granted because its guard permitted Unix socket creation in the checkout.
- The runtime now places Claude's native inbox socket in its writable home.
  A rebuilt image passed its Linux inbox protocol tests. A credential-free
  OpenShell probe running the revised investigation guard denied a socket bind
  in `/sandbox/worktree` with `EACCES` and allowed one in
  `/home/sandbox/.claude`; the probe sandbox was deleted. Paddock records guard
  version 2 and refuses resume or messaging for older guarded tasks.
- The first guard-version-2 Claude launch (`a499bc64fbf49533b884257a`) passed
  preparation but Claude exited before trust: it requires its socket's parent
  directory to be private, and trusted bootstrap had created `.claude` with
  mode `0755`. Preparation now sets mode `0700`. A second version-2 launch
  (`968611316b6af52da3ee91f1`) created the socket in that private directory
  and reached native workspace trust. The operator accepted trust, and Paddock
  observed `last_turn.state: inProgress`. Claude then reported a refused model
  connection. OpenShell logs showed its proxy could not read the non-dumpable
  trusted launcher's `/proc/<pid>/exe` while resolving the requesting binary;
  there was no pending policy proposal and the model provider was refreshed.
  The sandbox was stopped.
- Guard version 3 places launcher control files in private writable home and
  applies the same Landlock rules to the launcher before it starts Claude.
  App-backed Claude investigation `f601a7c947be213ab21e7106` launched in a
  rebuilt image, reached native workspace trust, and completed its model turn
  after the operator accepted trust. Its tool output shows file creation and
  Unix socket binding under `/sandbox/worktree` both failed with `Permission
  denied`; `git status --porcelain` was clean. Paddock reported
  `last_turn.state: completed`, with no commits, untracked files, or PRs. A
  follow-up with external ID `acceptance-20261112-claude-v3-followup-1` was
  delivered to the saved Claude session and completed a second clean `git
  status`; replaying that ID returned the same receipt without another message.
  Unused network proposals still made `blocked: true`, independently of the
  completed turns.
- A further guarded probe showed that `chmod` on a temporary worktree file
  still succeeds. Landlock enforces the tested file-content write denial but
  does not make worktree metadata immutable. The temporary file was removed.
  A task-local libseccomp prototype
  could resolve the relevant metadata syscalls, but the qualified guest's
  `seccomp_load` returned `ECANCELED`; no metadata filter was deployed.
- An isolated credential-free sandbox with `process.run_as_user: "1500"`
  confirmed ordinary exec and gateway upload both created files as UID 1500.
  Changing the workload UID therefore does not separate checkout ownership
  from the investigation agent. The exact probe sandbox was deleted and
  confirmed absent. OpenShell 0.1.6 also keeps filesystem and process policy
  fixed after workload start, as described in its
  [policy management guide](https://docs.nvidia.com/openshell/v0.1.6/how-it-works/policies/manage-policies).
- A rebuilt CLI read the Claude task's pinned native transcript
  inside OpenShell while its workspace trust prompt was open. `show --json`
  returned `last_turn.state: none`, without sending transcript content to the
  host. The same structural reader identified completed turns in five existing
  Claude session logs and an unfinished final prompt in a sixth; its focused
  fixture also covers queued follow-ups and partial transcript appends. The
  version-3 live task confirmed this signal across both the initial turn and a
  follow-up.
- A credential-free root-owned checkout image denied `chmod` and file creation
  for the `sandbox` user in Docker and in the qualified OpenShell guest, while
  home remained writable. OpenShell required the image's default working
  directory to remain writable; the checkout itself could be root-owned.
- Guard version 4 now builds a task-specific image from exactly the selected
  Git commit and ancestry. App-backed Codex investigation
  `59371608f017dd9d4df14cce` used pinned image
  `sha256:7a115286475d45d41ca573e0f042801b555f6cc3a45295a471fdab840f0da03d`.
  Its checkout and Git database were root-owned. The native model turn showed
  `chmod 600 README.md` failed with `Operation not permitted`, source `touch`
  failed with `Permission denied`, and `git status --porcelain` was clean.
  Paddock returned no commits, untracked files, or PRs. Older guard versions
  cannot resume or receive messages under the new runtime.
- A follow-up credential-free OpenShell probe made `/sandbox` root-owned and
  left `/sandbox/.paddock` writable for the agent. It denied both checkout
  rename and parent `chmod`. Guard version 5 applies and verifies that parent
  ownership, closing the remaining same-user helper-process rename path.
- App-backed Codex investigation `a81dabec08d5347fc5089ec7` used pinned
  image `sha256:e0f7d6d02adc93f88049b9ea23d8831efd00a1cfce482b2e83aae2cc8eb2a86c`.
  Its native turn reported file creation, Unix socket binding, `chmod`, and
  checkout rename denied, then a clean `git status --porcelain`. A separate
  unguarded OpenShell inspection confirmed UID 0 owns `/sandbox`, the checkout,
  and Git database; only `/sandbox/.paddock` was UID 1000 and writable.

Automated tests cover identity ownership, profile authorization, read-only
GitHub policy construction, immutable provenance, external dispatch
idempotency, message delivery receipts, separate per-identity key files,
manifest conversion, repository inventory, artifact retrieval, route ownership,
rotation that preserves the App actor and installation, and rejection of a
reused external ID with a different repository or execution choice. Verification
also exercises the exact scoped token permissions required for allowed profiles.
The full Go suite
passes with local HTTP fixtures.

The host-only agent, planner, and MCP execution paths now reject launches.
The internal locked-resume path and native message path also reject legacy host
task records. Legacy host watches report `unsupported` and leave new comments
pending, so a GitHub comment or follow-up cannot restart a host agent.
An initial task stopped before agent startup by an old host MCP grant is
eligible for a safe initial retry after that grant is removed. Artifact
retrieval also queries GitHub for closed or unwatched PRs on the task's exact
branch using its pinned App;
lookup failures are reported separately from local workspace artifacts.

## Remaining end-to-end checks

- Revalidate all execution paths after disabling host agents, Team Principal,
  model-backed planning, and trusted host MCP tools. The corresponding commands
  now fail closed; the App-backed Codex implementation path completed its test,
  commit, and draft PR.
- Recheck Claude's native turn and follow-up under a version-5 root-owned
  checkout. Its version-3 model path and version-4 Codex metadata boundary
  have passed independently.
- Define retention and distribution for task-specific investigation images
  beyond the local Docker-backed gateway.
