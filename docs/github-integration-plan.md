# GitHub coordination implementation plan

The current CLI has a file store with OS locking and atomic replacement, task-owned
worktrees, agent launch/exit tracking, and delegation. It has no planner, inbound
message delivery, PR ownership mapping, or event journal. Those are the missing pieces.

## End-to-end requirements

- Explicit task-to-PR watches; verify repository and branch ownership at registration.
- Poll both PR conversation comments and inline review comments, with pagination.
- Authorize the immutable GitHub author identity against a locally configured allowlist
  before planning. PR text never changes policy, task ownership, credentials, or routing.
- Shared local planner returns ignore, reply, message, or delegate. Ordinary discussion
  creates no work. Addressed questions go to the responsible agent; explicit requests
  for separate work use the existing delegate primitive.
- Separate GitHub transport identity from local task and agent identity. Development
  uses gh authentication. App mode reads an installation token from a named environment
  variable and uses a team-configured bot login; credentials never enter state files.
- Identify Paddock's outgoing comments and ignore them. App-authored comments are always
  ignored. Development mode must allow the developer's human comments while recognizing
  Paddock's own comments posted by that same account.
- Durable event records and an outbox. Claim effects before invoking external systems;
  do not replay an effect with an uncertain outcome after a crash. Reconcile GitHub
  replies through an event marker. Make ambiguous delivery visible for operator review.
- One-shot polling for laptops and an optional foreground loop; no hosted server,
  webhooks, Actions workflow, database, or permanent daemon.
- A reply command allows the responsible agent or engineer to report results through
  the configured GitHub identity and into the original review thread.

## Verification required before completion

Unit/integration checks must cover authorization, unrelated PR isolation, bot loops,
classification, pagination, reply threading, concurrent pollers, restart deduplication,
crashes before and after effects, and credential separation. Verify native agent message
delivery rather than merely recording an intent or typing into an approval prompt.

Create or identify a real PR associated with a Paddock task. Post an authorized actionable
review comment, poll it, verify the existing agent received it and produced a result,
and post a threaded reply through the configured identity. Post a non-actionable comment
and prove that it produces no work. Repeat polling in fresh processes and verify that
neither messages, delegated tasks, nor replies are duplicated. Record the PR/comment URLs,
task ID, event journal evidence, and exact checks in an acceptance report.
