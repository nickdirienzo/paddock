# GitHub coordination acceptance evidence

## Claude delivery — 2026-11-09

Native message routing now supports Claude in host and OpenShell tasks. Host
qualification used the installed Claude Code 2.1.286 in an already trusted
directory, with its existing login, workspace trust and tool permissions. A
disposable interactive session admitted the message through its Unix inbox,
persisted the exact enqueue record and replied `PADDOCK_HOST_INBOX_OK` in the same
conversation. No GitHub writes or source edits were made by the probe. A first
probe in an untrusted checkout stopped at the native trust dialog; trust was not
changed to make the test pass.

Automated coverage exercises macOS and Linux peer identity, stale/missing inboxes,
held/refused or mismatched enqueue evidence, host supervisor launch/resume cleanup,
whole-review delivery and deduplication in both runtimes, and routing validation
for both agents. The Linux OpenShell Python inbox tests also pass. Full Go race
tests, vet, and Linux/Windows builds pass. Windows compilation does not qualify
host Claude delivery; host peer verification supports macOS and Linux.

Host sessions launched before this change need a normal exit and resume. Their
comments remain pending until the inbox exists. Native host resume identity and
supervisor behavior have deterministic coverage; the live probe qualified initial
launch and message admission/reply, not a full GitHub round trip or native cold resume.

## Historical Codex checkpoint

This is the historical PR #1 checkpoint. See [App acceptance](github-app-acceptance.md)
for the subsequent native authentication and routing checks.

Verified on 2026-11-02 against the installed Codex CLI and a real GitHub PR.

## Live review round trip

- PR: https://github.com/nickdirienzo/paddock/pull/1
- Owning Paddock task: `84b5dff7d29d9d7b38a95c0a`
- Task branch: `paddock/84b5dff7d29d9d7b38a95c0a`
- Existing Codex conversation: `01a19d59-55ea-7751-9a52-12c5d4d78d75`
- Development GitHub identity: `nickdirienzo`, using local `gh` authentication.
- Authorized engineer identity: `nickdirienzo`, immutable GitHub user ID `723567`.

The task was launched before the PR was associated. Registration checked the task's
head branch and origin repository. Only this PR was watched.

1. A non-actionable conversation comment was posted:
   https://github.com/nickdirienzo/paddock/pull/1#issuecomment-6584029710
   Event `9ad459be8ff1a27c7c09098fc8b2a151` was recorded as `ignore` / `discussion` /
   `done`; no message or child task was created.
2. An actionable inline review comment requested a documentation change and validation:
   https://github.com/nickdirienzo/paddock/pull/1#discussion_r4445688929
   Event `a3b8ab560d97882275f0f281e8910fc0` was authorized, planned as `message` /
   `instruction`, queued to the existing Codex conversation, and recorded `done`.
3. Paddock posted a delivery acknowledgement in that same review thread:
   https://github.com/nickdirienzo/paddock/pull/1#discussion_r4445689469
4. The agent received the comment, edited `docs/github.md`, ran `git diff --check`,
   and used `paddock github reply` to report the result:
   https://github.com/nickdirienzo/paddock/pull/1#discussion_r4445693268
5. The parent agent inspected the actual worktree diff and preserved the change in
   commit `8127694` after the coding agent left it uncommitted as requested.
6. Two further CLI polling processes recorded both Paddock replies as ignored self
   comments, then found no new comments. Journal and GitHub inspection confirmed one
   message action and exactly two replies: acknowledgement and result. No duplicate
   coding action or reply was created.

The durable evidence is in the installation's `github.json` and task JSON record.
No credentials or local task contents are copied into this report.

## Automated coverage

- `TestPlanRequest`: ordinary discussion, explicit commands, questions, independent
  work, mention boundaries, and quoted non-commands.
- `TestGitHubRoundTripAndRestart`: native-queue adapter fixture, independent author
  authorization, ignored discussion, self-loop suppression, reopened-store deduplication.
- `TestGitHubPostReconciliation`: GitHub accepts a reply but its response is lost;
  reconciliation records the existing comment instead of posting again.
- `TestGitHubCrashClaimNeverReplays`: crash after a durable action claim leaves an
  uncertain outcome and causes no automatic redelivery.
- `TestGitHubUncertainReplyIsNotResent`: absent confirmation never causes blind reposting.
- `TestGitHubPolicyAndOwnership`: authorized external bot delivery, changed PR head
  ownership, planner output unable to bypass author checks, exclusive polling lock.
- `TestGitHubOnlyExplicitWatchesAndNoReplayOfEdits`: unwatched PRs cannot trigger work;
  editing an already-processed comment cannot replay it.
- `TestGitHubTransportPaginationIdentityAndThreading`: both comment endpoints, multiple
  pages, installation-token environment isolation, literal JSON bodies, review replies
  anchored to the root comment, and missing-token failures.
- `TestLiveGitHubDelegation`: a real terminal runs exactly one delegated child, with the
  correct parent and committed base; a new poller/store does not create another child.
- Full Go test suite, race detector, vet, and Linux/Windows compilation checks.

## Scope and practical limits

Native message delivery in this slice supports Codex. GitHub App installation tokens
are supported and transport-tested; the live acceptance used the permitted local `gh`
authentication path because no team App installation credentials were supplied.
Installation-token issuance and refresh remain external to Paddock.

Process death during a non-transactional external action can leave an uncertain result.
Paddock records that uncertainty and avoids replay rather than claiming distributed
exactly-once delivery. Runtime polling is an explicit command or foreground loop.
