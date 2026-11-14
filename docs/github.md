# GitHub coordination

Paddock can poll explicitly watched pull requests and route comments to their owning
local tasks. Codex uses its native session queue and Claude uses its native inbox
inside OpenShell. New OpenShell images pin Claude Code 2.1.325; 2.1.286 remains accepted. Claude's peer-message
admission and tool approvals remain in force. Delivery requires a persisted native
enqueue record; uncertain deliveries are held for inspection. Delegation uses the
existing task/worktree launcher.

## Configure a GitHub App

### Create a named Paddock identity and register its App

An identity is a logical Paddock principal. It has a local operating-system owner,
an allowed execution profile list, and optional GitHub credentials. The GitHub
actor, requester, and model-provider credentials are separate fields. Each
engineer can create a different identity; service workflows can use their own
identity owned by the local service account.

An unbacked identity can run local OpenShell tasks, including a repository
without a GitHub origin. It receives no GitHub actor, installation token,
push capability, or PR capability. Associate an App before using that identity
for GitHub work.

```sh
paddock identity create mozzie --profiles investigation,implementation
paddock identity github setup mozzie --app-name mozziebot --visibility any
```

`setup` starts a temporary loopback callback and prints a local URL. Open it in
your browser, review the App's name and requested permissions on GitHub, and
confirm registration. GitHub redirects to the callback with a one-time code;
Paddock exchanges it within GitHub's one-hour limit and imports the generated
private key into protected host storage. No permanent HTTP service is started.
Use `--owner ORGANIZATION` to register under a GitHub organization; omit it for
your personal account. `--visibility owner` is the default. Use `any` only if
the App needs installations outside the owning account.

The manifest requests Contents, Pull requests, and Issues `read` for an
investigation-only identity, or `write` if implementation is allowed. It disables
webhooks and user authorization. GitHub requires a human to install the new App
and select the repositories. After registration, `setup` prints the App install
URL, App ID, protected key path, and the commands to discover installations and
associate the selected installation. For example:

```sh
paddock github installations --app-id 12345 --private-key-file /protected/path.pem
paddock identity github associate mozzie --app-id 12345 \
  --installation-id 67890 --private-key-file /protected/path.pem
paddock identity verify mozzie --repo your-login/project
paddock identity github repositories mozzie --json
paddock identity show mozzie --json
```

Association verifies the App and installation with GitHub and derives the bot
login and numeric bot user ID. New OpenShell worktrees set their Git author to
`ID+login@users.noreply.github.com`, following [GitHub's noreply address format](https://docs.github.com/en/account-and-profile/reference/email-addresses-reference).
The App installation token authenticates pushes and PR creation as that bot.
`verify --repo` repeats the live authentication check, confirms the
installation can read that repository, and asks GitHub to mint a scoped token
for each profile the identity permits. It discards those tokens immediately.
`github repositories` lists the
installation's current repository access. Select the identity when
starting a task; the repository route is only a default for commands that omit
`--identity`:

```sh
paddock run --repo project --identity mozzie --profile implementation \
  --json 'Implement the change'
```

To use an App that already exists, create the identity and run `associate` with
its App ID, installation ID, and protected key file. `associate` requires an
unbacked identity; detach the old App before changing actors so its managed key
is removed. Use `identity github rotate` with a new key when keeping the same
App; rotation requires the same App, installation, and bot actor.
`identity github detach` clears
the Paddock binding; `identity disable` blocks new tasks and token renewal.
Revoking an App key or uninstalling the App itself is an action in GitHub's App
settings. Existing installation tokens can remain valid until they expire; revoke
them in GitHub when immediate cutoff is required. Inspect the selected App's
permissions and repository list in GitHub after installation or repair.

The registration sequence follows [GitHub's App Manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest).

### Existing direct App configuration

Each developer or team supplies its own App. Paddock has no central App registration
or hosted credential service. The App's slug determines the GitHub bot username.
Task ownership, authorized requesters, and the selected coding agent remain independent.

1. In your personal or organization settings, open **Developer settings → GitHub Apps →
   New GitHub App**. Choose the account that should own and administer the App.
2. Under **Repository permissions**, grant **Contents: Read and write**,
   **Pull requests: Read and write**, and **Issues: Read and write**. GitHub
   includes **Metadata: Read-only**. Contents write is required by the default
   OpenShell runtime so agents can clone repositories and push task branches.
   Pull requests permission covers PR comments and inline review reactions;
   GitHub documents Issues write permission for reactions to
   conversation comments. See [GitHub's reaction permissions](https://docs.github.com/en/rest/reactions/reactions#create-reaction-for-an-issue-comment).
   When updating an existing App, save the changes under **Permissions & events**
   and approve the new permissions for **each installation**. The added permissions
   do not take effect until the installing account approves them; see
   [GitHub's permission update instructions](https://docs.github.com/en/apps/maintaining-github-apps/modifying-a-github-app-registration#changing-the-permissions-of-a-github-app).
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
All polling and reply API calls use the selected App installation. In OpenShell,
agents also use ordinary `git` and `gh` to push branches, create PRs, and reply as
the mapped App bot. Paddock requests a task token restricted to its repository
with Contents, Pull requests, and Issues write permissions; the host retains the
App private key. See [OpenShell identity and messages](openshell.md#identity-and-messages).

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

- macOS: `~/Library/Application Support/paddock/credentials/github-app-APP_ID-IDENTITY.pem`
- Linux: `${XDG_CONFIG_HOME:-~/.config}/paddock/credentials/github-app-APP_ID-IDENTITY.pem`

The credentials directory has mode `0700`; key files have mode `0600`. Source files must
be regular files with owner-only access, not symlinks. The original downloaded file remains
in place. Named identities keep separate key copies even when they use the same App ID;
repeat `auth` or use `identity github rotate` with a new key to rotate one identity.
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
New agent execution takes place in OpenShell. The host broker retains App
private keys and supplies only repository-scoped, short-lived installation
tokens to App-backed task sandboxes.

### Legacy `gh` profile for GitHub coordination

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
The `gh` profile cannot start an OpenShell agent task. Use an App-backed identity
for GitHub work or an unbacked identity for local-only work.

## Watch a task's PR

Push the task branch and open its PR using your normal workflow, then register it:

```sh
paddock github watch https://github.com/your-team/project/pull/123 \
  --task TASK_ID --allow alice,bob
paddock github status --json
```

Registration verifies that the PR head branch matches the task branch and its head
repository matches the task's `origin`. Fork PRs can be watched when the task belongs
to the head repository. The local allowlist is resolved to immutable GitHub user IDs;
comment text and author-association labels cannot grant authorization. Re-run `watch`
to change allowed engineers. Only comments created after registration are considered.
Edited comments retain their event identity and are not executed again; post a new
comment to request another action.

To accept comments from **any GitHub account**, including future users and review bots, opt in explicitly:

```sh
paddock github watch https://github.com/your-team/project/pull/123 \
  --task TASK_ID --allow '*'
```

Quote the wildcard so your shell does not expand it. This permits any user or bot
that can comment on that PR, including non-collaborators on public repositories and
external reviewers such as Claude or Copilot. It is not a snapshot of the collaborator
list. Paddock's own App identity and Paddock-marked replies remain excluded to prevent
loops. The wildcard must be used alone. Existing watches remain restricted until you
update them; re-run `watch` with named user or bot accounts to narrow access.
Status exposes this policy as `allow_any_user: true`.
Legacy host task records report `unsupported` for comment delivery. Their
comments stay pending because those agents cannot be restarted outside
OpenShell.
After upgrading, restart any running poller with the upgraded CLI's `github stop`
and `github start` commands before registering wildcard watches. Registration
refuses an older running poller rather than saving a policy it cannot enforce.
The identity and OpenShell-only release also requires a version-7 poller before
new tasks launch. After that restart, watches for legacy host tasks remain
registered but cannot deliver comments; finish those tasks before cutting over
if their watches still need delivery.

Registering a watch starts or reuses **one background poller per registry**. It survives
the registering CLI and individual agents exiting. It does not start automatically at
login or reboot; run `paddock github start` or register a watch again.

```sh
paddock github start --interval 30s  # idempotent; existing interval is retained
paddock github status --json
paddock github stop                 # all registrations remain saved
```

For watched PRs, the poller appends ` ✓ merged` to the task’s existing Rex tab label (or session label for legacy records without a tab ID)
after GitHub confirms a merge, including when the agent is offline. It preserves
the current label and does not add the suffix twice. Closing a PR without merging
does not add it. Closed Rex sessions are not recreated; agents, native conversations,
and workspace retention are unaffected. Label update failures appear in the poller's
error status and are retried on later polls.

Status includes `poller.running` (checked against the process lock), PID, mode, interval,
last poll time/error, and log path. `watch_sessions` reports `online`, `resumable`, `offline`,
`unknown`, `missing`, or `unsupported` for each PR's task. A running service may still
report API errors; those are retried and don't terminate the service. To change the
interval or load an upgraded binary, stop and start it. Logs live in
`$PADDOCK_HOME/github-poller.log` (mode 0600).

Stopped Codex and Claude tasks with a saved conversation ID are reported as `resumable`. A new
authorized comment wakes the same task and conversation before native queue delivery.
If the worktree was collected, it is reconstructed from Git with an explicit recovery
notice. Missing history, uncertain launches, or unavailable terminals keep comments pending.
Tasks without a saved identity remain offline until explicitly resumed; no replacement
conversation is created. An exited agent's already queued replies
and delivery receipts can still be published. Native message routing supports
Codex and Claude in OpenShell. After upgrading the CLI, restart the shared
poller with `paddock github stop` followed by `paddock github start`. Paddock
never restarts a running agent or types into its terminal.

`paddock github poll` processes once. `paddock github poll --interval 30s` runs in the
foreground and takes the same singleton lease; it refuses to start if a poller is
already running. Ctrl-C stops that foreground process. One-shot poll/reply commands
still use their separate action lock, so they cannot overlap a background poll pass.
`github stop` requests cancellation by instance ID, without signaling a stored PID.

Pagination covers PR conversation and inline review comments. Closed PRs are skipped.
Inline requests include the original file and line context.
`paddock github unwatch PR_URL` removes that registration and retains its audit records.

## Conversation routing

With default direct routing, every new, nonempty comment from an approved author on a watched PR is queued to
that PR's existing task session. No mention, command prefix, keyword, or LLM routing
step is needed. `tests are failing`, questions, and ordinary discussion all reach the
same agent, which interprets them in context. Authorized external bot reviews are
delivered. Paddock's own App identity and marked comments are excluded before delivery.

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
`paddock github poll` publishes them; the shared background poller drains them regularly.
Engineers can use `--queue` explicitly, or omit it to post synchronously. A queued reply
prints its status rather than claiming a GitHub URL before publication. Paddock's own
App account and Paddock-marked comments never trigger work.
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

## Routing

Watched PR comments use direct delivery to the owning task. Model-backed
routing is unavailable until its planner runs in OpenShell. Existing watches
configured for planner mode should be changed to `--mode direct` before polling.

### Submitted reviews

A submitted review is delivered as one message containing its summary and inline
comments. Paddock groups by GitHub review ID, so the agent can consider shared
architecture concerns before changing individual lines. Draft reviews are not
delivered. Standalone PR comments and later inline-thread replies remain individual
messages. Comments drafted before watch registration are included if their review
is submitted afterward.

Each inline comment keeps its own reply target and receives 👀 after the grouped
message is queued. GitHub has no reaction endpoint for the review summary itself.
Persisted batch membership prevents replaying already delivered comments after a
restart or upgrade; uncertain delivery requires inspection instead of blind retry.
