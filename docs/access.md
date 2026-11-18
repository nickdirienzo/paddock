# Network access approval

Paddock exposes task-bound OpenShell network proposals for Arnold's Slack approval cards. The CLI uses `protocol_version: 1`; immutable network snapshots use `schema_version: 1`. Complete scope is displayed and sealed to a revision, while raw review tokens remain private host state.

## CLI contract

```sh
paddock access list --task TASK --status all --refresh --json
paddock access show REQUEST --task TASK --refresh --json
paddock access decide REQUEST --decision-file FILE --json
paddock access reconcile REQUEST --task TASK --json
```

The legacy operator `pending`, `show`, `approve`, and `reject` syntax remains as projections and wrappers over the same `access-v1` ledger. Structured RPC inspection is the only refresh path; no formatted CLI output is parsed and no second inbox is written. Existing `access/*.json` history is read-only compatibility input and cannot supply review tokens or grant authority. Desktop notification metadata is stored in the same ledger. Operator decisions use the same durable guarded executor as integrations. JSON failures retain a structured receipt whenever decision intent was recorded. Full task and request IDs are required by the versioned interface. The [wire schema and synthetic examples](access-wire-v1.json) define the fields.

The `integration_results` feature identifies Paddock-owned action qualification, complete plain-text `review` fields, and normalized `outcome` fields. A fresh list is required before enabling controls. Inspection failures retain cached requests for audit but never make them actionable. Requests include terminal records so old cards can be resolved after a restart. An authoritative `no_decision` result means the known request has no unresolved decision intent; the integration may retry its exact frozen input and external ID. Unknown requests, incomplete ledgers, and uncertain observations never prove absence of intent.

An integration renders `review.summary` and `review.details` without shortening the reviewed scope. It validates identity, freshness, and decision attribution, then consumes `outcome.state` instead of reimplementing backend states:

- `unresolved`: retain the receipt and reconcile; never infer failure or repeat a mutation.
- `settled`: automatic recovery is complete; the summary still distinguishes grant, application, and notification status, including outcomes needing operator inspection.
- `review_ended`: this attempt is stale/conflicting and did not acquire the request. A different frozen decision key may be submitted through the same guarded engine.
- `not_submitted`: authoritative absence of intent for a known request; only the exact frozen input/key may be retried.

These fields are derived for responses and do not create a second persisted source of decision state. Storage/uncertain-outcome errors remain unresolved even when an in-memory receipt reports success.

## Exact reviewed rule

Qualified approval has semantics `exact_reviewed_rule`: it applies the exact displayed credential-free REST network rule to the recorded sandbox, with the captured review token. It does not freeze independent administrator changes to global/provider policy. The policy map key, full rule, task and actual sandbox identity are sealed into the stored revision.

The initial adapter accepts explicit executable scope and exact-host inspected REST endpoints. Credential bindings, signing, provider/token-grant authority, unsafe or unknown fields, flagged proposals, and destinations overlapping credential authority remain unavailable through this path. See [the network guarantee](access-network-guarantee.md).

Stock OpenShell 0.1.6 rejection can also revoke an approved rule. Because it cannot atomically reject only a pending proposal, the adapter does not expose that rejection action. Unsupported controls remain unavailable; the operator can inspect them explicitly. No host credentials or other host execution are granted by network approval.

## Authentication and lifecycle

The local OS caller is authenticated separately from the Slack actor asserted by Arnold. Decisions are scoped to caller, task and external ID, and bind the complete input; identical retries return the same receipt, changed payloads conflict. One durable winner owns each request. After an ambiguous backend outcome, reconciliation observes state and does not blindly reissue mutations or replace tokens.

Grant approval, runtime policy application, and native notification are separate states. Notifications use stable native message receipts; pending messages can retry, while claimed/uncertain messages require inspection. Native delivery may resume the same saved conversation after idle expiry, but never creates a replacement task or follows a new sandbox incarnation. Target-loss evidence is rechecked under the actual messaging lease. Terminal disposition cannot hide an already claimed, delivered, or uncertain receipt.

Pending proposals and unresolved decisions/notifications retain the workspace. Pending proposals do not retain a safely completed agent process. A runtime poller reconciliation pass services durable notification outboxes for stopped retained tasks. Repository trust and tool configuration use the existing controls, separately from task-bound network approval.

## Agent instructions and validation

Every prepared sandbox receives `/sandbox/.paddock/access-guide.md`, discoverable with `paddock --help`. Paddock adds the same guide pointer to initial launch instructions and authored `paddock message` deliveries. Keyed authored deliveries freeze `delivery_body` at admission; retries preserve it across upgrades while `body` remains the original idempotency input. Internal notifications and legacy receipts keep their original payloads. No extra guidance poller or startup notice is used.

The guide gives a fixed `policy.local` submission and bounded-wait fallback if OpenShell's own advisor guide is missing. Requesting review grants nothing; the agent waits for `policy_reloaded` before retrying work.

Automated tests use disposable registries, structured RPC fixtures and deterministic native message delivery. No live grants or deployment settings are changed. Live qualification must separately verify the deployed mTLS/API configuration, runtime policy load, a real dependency installation, and Slack interaction authorization.
