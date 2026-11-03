# Orchestration acceptance — 2026-11-02

Executed `scripts/orchestration-acceptance.py` against the real Codex subscription
CLI and connected Rex server, with an isolated registry and two local Git repositories.
No classifier stub or fake agent was used in this exercise. The planner used the Codex
CLI default model; a separate configuration probe explicitly selected `gpt-6-astra`.

Local evidence directory: `/private/tmp/paddock-orchestration-live`.
Each case has a JSON signal record, decisions/rationale, requested context, and saved
execution outcomes. The runner replayed the same source/ID/body and asserted that the
entire record remained unchanged and that no extra tasks appeared.

| Requirement | Observed evidence |
| --- | --- |
| Irrelevant comment creates no work | `acceptance-irrelevant` chose `ignore`; no message/task action |
| Action reaches the appropriate existing agent | `acceptance-existing-specialist` inspected the documentation agent's Rex session, then messaged it despite the signal originating from the API task; its README acquired the requested health-response example |
| Independent work creates a related child | `acceptance-independent-child` created task `411ee0de395accd348c6c390`, parent `0d3ef948b9e28413ae280728`, with a distinct worktree/session; `HEALTH-TEST-PLAN.md` was subsequently written there |
| Work beyond the origin creates a separate task | `acceptance-separate-work` created root task `27c26a4f45683b81a60c19b0` in `garage-docs`, with no parent and a durable relation to the API origin task |
| Repeated processing does not duplicate actions | All five signals replayed with byte-equivalent decoded records and unchanged task ID sets; four tasks total |
| Orchestrator exits without stopping coordinated agents | After all orchestration commands returned, `paddock tasks --json` observed all four tasks and their terminals running |
| Reply without implementation | `acceptance-reply-only` read the documentation session and returned an answer without launching or messaging an agent |
| Operator model and prompt configuration | `configured-model-probe` in `/private/tmp/paddock-orchestration-acceptance` recorded `gpt-6-astra` and the configured instructions, then returned the requested routing preference in a dry-run `reply` action |

The independent `garage-docs` coding agent encountered upstream HTTP 429 responses.
A follow-up was queued to that same session; it hit the same rate limit. Its task,
worktree, and agent remained present, and no replacement task was launched. This
validates orchestration and lifecycle behavior, not completion of that agent's
`CONTRIBUTING.md` assignment. The parent and existing documentation agent remained
available. The child initially questioned whether it needed another task, inspected
its Paddock metadata, and correctly worked in its existing child worktree. The launch
handoff now explicitly says that its task, worktree, and session already exist.

The live exercise uses CLI signals against real Paddock tasks. GitHub integration is
covered by deterministic tests for author/bot filtering, cross-task routing while the
origin is offline, event-scoped reply authority, reply-only/ignore behavior, and
unchanged direct-delivery receipts/replay guarantees. It does not claim to have posted
live GitHub comments for this acceptance run.

Other verification: full Go test suite and vet; Linux amd64 and Windows amd64 builds;
per-signal concurrency, crash ambiguity, revoked scope, dry-run isolation, bounded
context reads, configuration precedence, and exclusion of API/GitHub credentials from
the planner process. Windows was cross-compiled, not exercised on a Windows host.
