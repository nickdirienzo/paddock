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
  (`4c0f3f80aece9d64240c3d6b`) launched and created its native inbox socket;
  Claude is awaiting the operator's workspace trust decision before its first
  model turn.
- A further guarded probe showed that `chmod` on a temporary worktree file
  still succeeds. Landlock enforces the tested file-content write denial but
  does not make worktree metadata immutable. The temporary file was removed;
  strict metadata isolation remains open. A task-local libseccomp prototype
  could resolve the relevant metadata syscalls, but the qualified guest's
  `seccomp_load` returned `ECANCELED`; no metadata filter was deployed.
- A rebuilt CLI read the still-running Claude task's pinned native transcript
  inside OpenShell while its workspace trust prompt was open. `show --json`
  returned `last_turn.state: none`, without sending transcript content to the
  host. The same structural reader identified completed turns in five existing
  Claude session logs and an unfinished final prompt in a sixth; its focused
  fixture also covers queued follow-ups and partial transcript appends. A live
  Claude model turn remains pending human workspace trust.

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
- Finish the Claude investigation after native workspace trust and verify its
  write denial, clean worktree, and follow-up behavior.
- Prevent investigation agents from changing checkout metadata such as file
  modes while preserving writable home and build scratch space.
- Verify Claude's new native transcript turn-completion signal in a live
  OpenShell task after the operator accepts the workspace trust prompt. The
  parser uses structural events only and keeps transcript content in the
  sandbox.
