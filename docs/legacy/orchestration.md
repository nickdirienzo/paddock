# Ephemeral orchestration

> Model-backed orchestration is currently unavailable. Its former planner ran
> on the host; all LLM execution now requires OpenShell. The material below
> describes the retired design and should not be used as current setup guidance.

Paddock's orchestration entry point runs **Signal → Perceive → Plan → Act → Exit**.
A subscription-backed Codex CLI chooses structured actions. Paddock validates and
executes them through the existing task launcher, native message queue, and GitHub
reply outbox. The planner exits; the agents it coordinates keep their own lifetimes.
There is no new scheduler, daemon, database, hosted service, or workflow engine.

## Model and planning instructions

Use an installed Codex CLI with an existing ChatGPT login. Planning does not require
or accept a direct model API key. The planner uses `codex exec --ephemeral`, structured
output, a read-only sandbox, and disabled command tools, hooks, plugins, apps, and
subagents. User config and execpolicy rules are not loaded. The existing login remains
available through `CODEX_HOME`. The required flags are verified with Codex 0.159.2;
older versions without these flags fail visibly rather than weakening isolation.

```sh
paddock planner configure --model MODEL --instructions-file ./pit-wall.txt
paddock planner show
paddock planner reset
```

`MODEL` is a model ID available to your Codex subscription. If unset, Codex chooses its
CLI default. This setting controls planning only, not the models of launched coding
agents. An instructions file might contain:

```text
Prefer the agent already working on the relevant subsystem.
Keep small follow-ups in existing sessions.
Use durable children for independent implementation that needs a separate worktree.
For status questions, inspect the session and reply without assigning implementation.
```

The file's contents are copied into `$PADDOCK_HOME/planner.json`, mode 0600. Editing the
original file does not silently change policy; rerun configure to load it. Instructions
supplement the fixed planning protocol. They cannot change action validation, target
scope, author authorization, or replay protection.

Model precedence: `orchestrate --model` or a GitHub watch's model override, then the
registry configuration, then the Codex CLI default. `orchestrate --instructions-file`
overrides registry instructions for that invocation. Signal records retain the model
setting, operator instructions, decisions, rationale, requested context, and outcomes.
Changing configuration does not re-plan an action already committed to a signal.

## CLI signals and previews

```sh
paddock orchestrate --id support-104 --task ORIGIN_TASK \
  --tasks OTHER_TASK --repos docs-repo --dry-run \
  "Find the documentation owner and ask them to document the new health response."

# Use the same signal ID and body to execute after inspecting the preview.
paddock orchestrate --id support-104 --task ORIGIN_TASK \
  --tasks OTHER_TASK --repos docs-repo \
  "Find the documentation owner and ask them to document the new health response."

paddock orchestrate show cli support-104
```

The origin task and its repository are included automatically. `--tasks` explicitly
allows other existing tasks; `--repos` explicitly allows new work in additional
repositories, using registered aliases or paths. The model sees full task IDs and
canonical repository paths. A separate task can live in a different repository and is
linked back through `signal_id` and `related_task_id`, without acquiring a parent.
A delegated child also records `parent_id` and inherits the parent's repository.

`--json` emits the complete signal record. A dry run perceives and plans normally but
never sends a message, launches a task, or queues a reply. It saves an inspectable
preview without consuming the signal. A subsequent live invocation plans again using
current context. Once execution starts, repeated invocations reuse the saved actions.

For scripts, `--signal-file` accepts a JSON object:

```json
{"source":"cli","id":"support-104","body":"Investigate the failing health check","task_id":"FULL_TASK_ID"}
```

The CLI only accepts `source=cli`; it cannot impersonate an authenticated GitHub event.
Other integrations can invoke `Orchestrator.Process` with their own source and stable
ID after authorizing the signal. Source adapters are responsible for establishing
operator-approved scope; model output and incoming comment text are never policy.

## Perception and actions

The initial catalog includes scoped task prompts, repository ownership, branches,
agent types, session availability, and watched PRs. The model can request additional
context before deciding:

| Context | Read operation |
| --- | --- |
| `task` | Full task prompt, ownership, status, conversation ID, and error |
| `session` | Bounded recent output from that task's existing Rex terminal |
| `repo` | Recent commits, top-level tracked files, and committed README |
| `pr` | Current PR metadata and recent discussion/review comments through its mapped identity |

Reads are performed by Paddock using fixed operations. The model cannot choose shell
commands, arbitrary file paths, HTTP endpoints, or credentials. At most four context
rounds precede the final decision; each round can request at most six reads. Context
errors and truncation are visible to the planner and persisted. Session capture
currently requires Rex; unavailable context is reported rather than invented.

The planner selects one to four non-overlapping actions:

| Action | Effect |
| --- | --- |
| `ignore` | Record why no work is needed; must stand alone |
| `message` | Queue context or instructions to a scoped, existing Codex task |
| `delegate` | Launch a durable child with its own worktree and agent |
| `create_task` | Launch independent work in an authorized repository |
| `reply` | Return text to the CLI or queue a GitHub reply using the originating event |

All actions are validated before any execution. Message destinations must support the
native Codex queue. New agents may be Codex or Claude; they use the ordinary Paddock
launch settings and trust flow. A child may use its parent's branch/base commit, while
independent work starts from the repository's default base. Neither planner output nor
operator instructions can add an arbitrary command or executable.

## GitHub integration

Existing watches retain direct-to-session behavior by default. This path now uses the
same durable signal executor without invoking the model. It forwards the original
comment once, with its author, URL, file/line context, and reply command.

After registering a watch, explicitly enable planning and its scope:

```sh
paddock github routing https://github.com/OWNER/REPO/pull/123 \
  --mode planner --tasks OTHER_TASK --repos docs-repo --model MODEL

# Restore direct routing for subsequent signals:
paddock github routing https://github.com/OWNER/REPO/pull/123 --mode direct
```

The existing author allowlist and immutable GitHub account IDs are checked before model
invocation and again before execution. Authorized external bots can provide signals;
Paddock's own App identity and marked comments are ignored.
A planner-enabled watch can route to another authorized task even when its original
agent is offline. A direct watch still waits for its owning session. Watch changes do
not replay already-consumed comments; old uncertain deliveries are not automatically
retried. Re-registering a watch preserves its routing configuration.

GitHub event IDs and source-qualified signal IDs connect the event audit to the shared
executor. The direct path and planned path cannot both deliver the same event.
Successfully messaged tasks receive authority to reply to that specific source event;
the original watch, author policy, and mapped GitHub identity still govern publication.
New task recipients receive the same narrowly scoped reply command. The model never
receives App credentials.

An eyes reaction still means confirmed native message queue delivery. Ignored signals,
offline destinations, and uncertain deliveries get no reaction. Task creation and
reply-only decisions are visible in their saved outcomes; they do not claim a message
was delivered to the originating agent. The existing shared poller drains queued replies.

## Durability and failures

Records are atomic JSON files under `$PADDOCK_HOME/signals/`, keyed by a hash of source
and stable signal ID. A per-signal OS lock prevents concurrent processing. Reusing an
ID with different content fails. Completed actions are never automatically repeated.
Planning failures can be retried because planning has no execution capabilities.

Before each external action, Paddock persists `executing`, including a reserved task
ID for launches. A crash during an action becomes `uncertain` on recovery; inspect the
record and destination before taking further action. Paddock favors no duplicate work
over blindly retrying an ambiguous queue or launch. Confirmed offline delivery remains
`pending`, with the plan intact; retrying the signal resumes it when the same destination
is available. CLI signals require explicit re-invocation; GitHub pending signals are
revisited by the existing poller. No scheduler is added.

## Live acceptance

Build the CLI and run the opt-in live exercise with a connected Rex server and logged-in
Codex CLI:

```sh
mise run build
python3 scripts/orchestration-acceptance.py --root /private/tmp/paddock-live
```

It creates two local Git repositories and real Paddock/Codex sessions, exercises all
five action types, replays every signal to check deduplication, and checks that all
four durable agents remain running after the orchestration commands exit. It saves
JSON records and terminal captures under the supplied root and leaves sessions
available for inspection. It creates no PRs, pushes, or external comments. Re-running
with the same root reuses saved IDs and results instead of launching replacement tasks.

The model runner uses the documented [Codex non-interactive interface](https://learn.chatgpt.com/docs/non-interactive-mode)
and [configuration controls](https://learn.chatgpt.com/docs/config-file/config-reference).
