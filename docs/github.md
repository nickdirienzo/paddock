# GitHub coordination

Paddock can poll explicitly watched pull requests and route comments to their owning
local tasks. The initial native message adapter uses Codex's session queue. Claude
can still own ordinary Paddock tasks, but cannot receive GitHub messages through this
adapter; delivery is recorded as uncertain with an explicit error rather than typed
into its terminal. Delegation uses the existing task/worktree launcher.

## Configure a GitHub App

Each developer or team supplies its own App. Paddock has no central App registration
or hosted credential service. The App's slug determines the GitHub bot username.
Task ownership, authorized requesters, and the selected coding agent remain independent.

1. In your personal or organization settings, open **Developer settings → GitHub Apps →
   New GitHub App**. Choose the account that should own and administer the App.
2. Grant **Pull requests: Read and write** and **Issues: Read and write**. GitHub
   includes **Metadata: Read-only**. Pull requests permission covers PR comments and
   inline review reactions; GitHub documents Issues write permission for reactions to
   conversation comments. See [GitHub's reaction permissions](https://docs.github.com/en/rest/reactions/reactions#create-reaction-for-an-issue-comment).
   Existing installations must accept added permissions before those reactions work.
   No organization permissions are needed.
3. Disable **Active** under Webhook. Leave OAuth callbacks and user authorization disabled.
   Choose **Any account** if the App needs installation outside its owning account.
4. Create the App, generate a private key, and install it on the repositories Paddock
   should access. Record the App ID. A client secret is not used.
5. Protect the downloaded key, discover its installations, and configure a named profile:

```sh
chmod 600 /path/to/app.private-key.pem
paddock github installations --app-id 12345 --private-key-file /path/to/app.private-key.pem
paddock github auth --name personal --mode app \
  --app-id 12345 --installation-id 67890 \
  --private-key-file /path/to/app.private-key.pem
paddock github route 'your-login/*' --identity personal
```

Authentication verifies the App and installation against GitHub, derives the bot login,
and mints an installation token. An optional `--login 'your-app[bot]'` asserts the expected
principal. Paddock signs JWTs and manages tokens itself; App mode does not require `gh`.
All polling and reply API calls use the selected App installation. Future PR creation
can use this same transport; Paddock does not yet create PRs. Pushing Git refs under the
App would additionally require Contents write permission and a Git credential integration.

### Map personal and team repositories

Configure another profile using its own App ID, installation ID, and key:

```sh
paddock github auth --name mirage --mode app \
  --app-id 23456 --installation-id 78901 \
  --private-key-file /path/to/mirage.private-key.pem
paddock github route 'mirage-security/*' --identity mirage
paddock github route 'your-login/shared-project' --identity mirage
paddock github identities
paddock github status --json
```

Resolution uses an exact `owner/repository` route first, then `owner/*`, then an explicitly
configured `'*'` route. An unmapped repository fails with a configuration error. Quote
wildcards so the shell does not expand them. Use `github unroute PATTERN` to remove a route.
Routes select credentials; GitHub's installation permissions still determine actual access.
Installing one App into multiple accounts retains the same bot username. Use distinct Apps
for distinct personal and team bot identities; profiles may also share one App with different
installation IDs.

New replies select the repository's current profile. Once queued or attempted, replies
retain their original profile, App ID, installation ID, and bot login even if routes change.
Replacing or removing that profile blocks its pending replies rather than changing their
principal. Rotating a key for the same App and installation preserves pending replies.

### Credential storage and headless use

File-based setup imports an owner-only copy into the OS user config directory:

- macOS: `~/Library/Application Support/paddock/credentials/github-app-APP_ID.pem`
- Linux: `${XDG_CONFIG_HOME:-~/.config}/paddock/credentials/github-app-APP_ID.pem`

The credentials directory has mode `0700`; key files have mode `0600`. Source files must
be regular files with owner-only access, not symlinks. The original downloaded file remains
in place. Keys are shared by App ID across profiles; repeat `auth` with a new key to rotate.
This uses protected files, not macOS Keychain. Windows currently supports environment PEM
configuration; file import requires Unix permissions.

For portable/headless use, provide these variables through your secret manager:

```sh
export PADDOCK_GITHUB_APP_ID=12345
export PADDOCK_GITHUB_INSTALLATION_ID=67890
# PADDOCK_GITHUB_PRIVATE_KEY contains the PEM, injected by your secret manager.
paddock github auth --name personal --mode app
paddock github route 'your-login/*' --identity personal
paddock github poll
```

An environment profile reads `PADDOCK_GITHUB_PRIVATE_KEY` on each signing operation;
it must remain available to the polling process. The PEM is never copied into state.
This single environment variable supplies one App key per process; use imported files
for multiple distinct Apps in the same poller. Alternatively,
`PADDOCK_GITHUB_PRIVATE_KEY_FILE` supplies a protected file to import during `auth`.
Do not provide both a file and inline PEM. Flags override the corresponding ID/file
variables. No secrets belong in repository config, `mise.toml`, or task prompts.

Installation tokens exist only in the HTTP transport's memory. Paddock renews them within
one minute of expiry and refreshes once after a confirmed HTTP 401. It never blindly
retries a mutation after a timeout or server error. Each profile's transport has its own
cache. See [GitHub's installation token flow](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app).

App PEM variables and installation tokens are removed from agent, terminal, Git, and
agent-control subprocess environments. They are absent from task state and generated
prompts. App-backed agent replies enter a local queue; the credentialed poller sends them.
Host execution still shares OS permissions with other processes running as the same user:
this prevents credential inheritance, but does not sandbox an agent that deliberately reads
owner-readable files. Future sandbox execution can provide that stronger boundary.

### Optional development fallback

```sh
paddock github auth --name dev --mode gh --login your-login
paddock github route 'your-login/test-repo' --identity dev
# Only if you explicitly want gh for every otherwise-unmapped repository:
paddock github route '*' --identity dev
```

Named profiles do not create a default route. Omitting `--name` explicitly configures
`default` and maps `'*'` to it for compatibility. Existing single-identity state migrates
to that same mapping, preserving the original principal for pending replies. Old App
profiles using `--token-env` must be reconfigured with an App ID, installation ID, and key.

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
Inline review requests delivered to the owning agent include the original file and line context.
`paddock github unwatch PR_URL` stops polling that PR and retains its audit records.

## Conversation routing

Every new, nonempty comment from an approved author on a watched PR is queued to
that PR's existing task session. No mention, command prefix, keyword, or LLM routing
step is needed. `tests are failing`, questions, and ordinary discussion all reach the
same agent, which interprets them in context. Bot accounts and Paddock-generated
comments are excluded before delivery.

Mentions and old `paddock:` command-like text are passed through as comment text.
The poller does not create child tasks, answer status questions, or execute instructions
based on wording. Nothing is automatically committed, pushed, merged, or turned into
another PR.

Native messaging resolves the Codex session by its exact task worktree, remembers the
conversation ID, and refuses ambiguous matches or tasks without confirmed running
terminals. It uses `codex queue`, not terminal keystrokes. This requires a Codex version
supporting `queue` and the app-server `thread/list` protocol. The local agent's existing
permissions remain in effect.

After confirmed session delivery, the poller adds an 👀 reaction to the original
conversation or inline review comment. This is a deterministic receipt that the comment
was queued, not a claim that the agent has read it or completed the work. There is no
extra delivery-acknowledgement comment. Failed or uncertain delivery gets no reaction.

Reaction status is stored separately as `receipt` and `receipt_error` in
`github status --json`. Failed reactions remain pending and are retried without
redelivering the comment; GitHub returns the existing reaction if a previous request
succeeded but its response was lost. Interval polling reports reaction errors and keeps
running so a missing reaction permission does not stop delivery or result replies.
One-shot polling exits with an error if receipts remain pending.
To report the result, the agent or engineer can run:

```sh
paddock github reply EVENT_ID "Fixed the nil check; the regression test passes."
```

The delivered message includes the event ID and an absolute reply command. An agent
with `PADDOCK_TASK_ID` set can reply only to events owned by its task. Replies use the
repository's mapped GitHub identity. In App mode, replies from a task process
(`PADDOCK_TASK_ID` set) are queued automatically without reading the key. The next
`paddock github poll` publishes them; a foreground polling loop drains them regularly.
Engineers can use `--queue` explicitly, or omit it to post synchronously. A queued reply
prints its status rather than claiming a GitHub URL before publication. Bot accounts and Paddock-marked comments never trigger work.
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
