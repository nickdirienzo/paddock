# GitHub coordination

Paddock can poll explicitly watched pull requests and route comments to their owning
local tasks. The initial native message adapter uses Codex's session queue. Claude
can still own ordinary Paddock tasks, but cannot receive GitHub messages through this
adapter; delivery is recorded as uncertain with an explicit error rather than typed
into its terminal. Delegation uses the existing task/worktree launcher.

## Configure an identity

For development, use the GitHub account already authenticated by `gh`:

```sh
paddock github auth --mode gh --login your-login
```

For a team's own GitHub App, install the App on the watched repositories and supply
an installation access token through an environment variable:

```sh
paddock github auth --mode app --login 'your-team-bot[bot]' --token-env TEAM_GITHUB_TOKEN
```

The App needs repository metadata read access, pull requests read/write access, and
issues read/write access for conversation comments. Supply and refresh the installation
token outside Paddock; token expiry is reported as an API error. Paddock stores only the
variable name and identity metadata. It never stores token values, private keys, or
installation secrets. Authentication is isolated in the GitHub transport; changing
from a developer identity to an App does not change task ownership or execution identity.
The configured App login must match the App issuing the installation token.

## Watch a task's PR

Push the task branch and open its PR using your normal workflow, then register it:

```sh
paddock github watch https://github.com/your-team/project/pull/123 \
  --task TASK_ID --allow alice,bob
paddock github poll
paddock github status --json
```

Registration verifies that the PR head branch matches the task branch and its head
repository matches the task's `origin`. Fork PRs can be watched when the task belongs
to the head repository. The local allowlist is resolved to immutable GitHub user IDs;
comment text and author-association labels cannot grant authorization. Re-run `watch`
to change allowed engineers. Only comments created after registration are considered.
Edited comments retain their event identity and are not executed again; post a new
comment to request another action.

One-shot polling exits after processing available comments. To poll while your laptop
is available, run `paddock github poll --interval 30s`; stop it with Ctrl-C. There is no
webhook, hosted service, GitHub Actions workflow, or permanent daemon. Pagination covers
both PR conversation comments and inline review comments. Closed PRs are skipped.
`paddock github unwatch PR_URL` stops polling that PR and retains its audit records.

## Conversation routing

Authorization and self-comment filtering happen before planning. The shared local
planner has a deliberately small vocabulary; the coding agent handles freeform reasoning:

| Comment | Action |
| --- | --- |
| `Looks good, thanks!` | Record as discussion; create no work. |
| `Please fix the nil check.` | Queue a message for the PR's existing task agent. |
| `Can you add a regression test?` | Queue an actionable instruction. |
| `@your-bot why does this happen?` | Queue an addressed question. |
| `paddock: message: investigate this race` | Explicitly message the task agent. |
| `paddock: delegate: add independent documentation` | Create a child task using the existing launcher. |
| `paddock: status` | Reply with the recorded task status. |
| `paddock: reply: Thanks, investigating.` | Post the explicitly requested text. |

The `paddock:` command prefix is independent of the team's GitHub username. Ordinary
unaddressed questions and unsupported wording remain discussion; use an explicit
message command when intent could be ambiguous. Delegated work starts from committed
parent-branch work, in a separate worktree. Nothing is automatically committed, pushed,
merged, or turned into another PR.

Native messaging resolves the Codex session by its exact task worktree, remembers the
conversation ID, and refuses ambiguous matches or tasks without confirmed running
terminals. It uses `codex queue`, not terminal keystrokes. This requires a Codex version
supporting `queue` and the app-server `thread/list` protocol. The local agent's existing
permissions remain in effect.

A successful action posts an acknowledgement to the originating review thread (or PR
conversation). That acknowledgement means delivery/delegation, not completed coding work.
To report the result, the agent or engineer can run:

```sh
paddock github reply EVENT_ID "Fixed the nil check; the regression test passes."
```

The delivered message includes the event ID and an absolute reply command. An agent
with `PADDOCK_TASK_ID` set can reply only to events owned by its task. Replies use the
configured GitHub identity. Bot accounts and Paddock-marked comments never trigger work.
In development mode, the same account can write human comments and Paddock replies;
the marker distinguishes outgoing replies without ignoring all of the developer's input.

## Durability

Watches, author IDs, event decisions, results, and the reply outbox are versioned JSON
in `PADDOCK_HOME/github.json`. Writes use Paddock's existing OS lock and atomic replacement.
A separate `.github-poll.lock` serializes polling and replies without holding the task
registry lock during network calls. No database or worker infrastructure is added.

Each source comment has a stable event ID. An action is claimed durably before delivery
or delegation. Completed and ignored events are not repeated after restarts. If the
process dies during an external action, the next poll marks it `uncertain` and does not
replay it. Inspect the task/session before manually issuing another instruction.
This favors avoiding duplicate work over automatically retrying ambiguous delivery.

Replies have deterministic IDs and hidden markers. If GitHub accepted a reply but its
response was lost, the next poll finds that marker and records the existing reply. If
no matching reply can be confirmed, Paddock reports uncertainty instead of blindly
posting again. Identical explicit replies for the same event are idempotent. Different
reply text is a new update. Neither a failed acknowledgement nor a network retry causes
the original coding action to be repeated.

These are at-most-once action attempts, not a distributed exactly-once transaction.
Deleted local state loses deduplication history; keep the registry on a local filesystem.
