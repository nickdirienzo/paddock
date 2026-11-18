# Exact reviewed network rule

The usable contract is `exact_reviewed_rule`: Paddock approves the captured
credential-free, inspected REST AddRule for the same immutable sandbox. It does
not promise a transaction freezing the whole gateway, future policy, or all
independently administered provider/global inputs. Production code advertises
Approve for individually qualified proposals; it is not an informational-only UI.

This is the chosen path. No upstream dependency patch is required for the
untrusted rule-substitution threat identified below. A stronger full-context or
atomic decision-metadata guarantee would need upstream work.

## Pinned source evidence

Reviewed OpenShell commit `ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47` (0.1.6).
The locally cached policy/replay sources were checked against their Git blob IDs
in that commit's tree, not inferred from an unversioned checkout.

| Boundary | Source evidence | Consequence |
| --- | --- | --- |
| Rule identity | [policy.rs:729](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L729), `compute_proposal_review_token` | Token covers rule map key, canonical rule bytes, current/candidate policy hashes and credential fingerprints. |
| Reviewed precondition | [policy.rs:1747](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L1747), `require_current_proposal_evaluation` | Supplied token must match stored token; live reevaluation must match too. Paddock requires a nonempty captured token and never uses the legacy empty-token path. |
| Captured application | [policy.rs:6042](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L6042), [policy.rs:7777](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L7777) | Handler captures a chunk once. Merge decodes that same captured rule and uses that captured map key; it does not reread an editable row to choose what to grant. A later edit cannot replace those applied bytes. |
| Sandbox identity | [policy.rs:452](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L452), [policy.rs:6087](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L6087) | Chunk ownership must match the resolved immutable sandbox ID; persistence uses that captured ID. Name reuse cannot redirect an old chunk to a new sandbox. The token itself does not include the sandbox ID. |
| Base-policy conflict | [policy.rs:7625](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L7625) | Merge checks the expected effective hash using captured provider layers and rejects a conflicting policy-version write. It does not silently rebase approval on a new reviewed rule. |
| Untrusted submission | [policy.rs:5800](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L5800), [policy.rs:6577](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/policy.rs#L6577) | Agent proposals receive server-generated IDs. Existing-chunk editing is an Admin operation and produces a new evaluated payload/token. Mechanistic refresh can change a row, but cannot replace the handler's captured rule. |
| Replay identity | [ordinary.rs:962](https://github.com/NVIDIA/OpenShell/blob/ac7f615cd12d5cc49cb16eb1f5f10ffeb1e90f47/crates/openshell-server/src/grpc/mutation_replay/ordinary.rs#L962) | Replay restores the captured sandbox result and checks that object's liveness; it does not grant again by resolving a reused name. |

Paddock adds its own complete snapshot digest, including rule map key and target,
checks fresh inspection against the frozen revision, sends only the stored token,
and durably records a single winner plus stable upstream request UUID. The Go
adapter's production constructor advertises `ExactRuleBound=true` for this narrow
meaning; per-rule qualification still determines whether an Approve button exists.
Credentials/signing/provider authority, overlapping credential destinations,
uninspected protocols, unsupported fields and flagged proposals are excluded.

## Deliberate limits

Approval is not one atomic transaction covering chunk metadata and all evaluator
inputs. After token validation the handler separately reloads provider context;
merge uses captured provider layers with `atomic_context=None`. Independent
trusted administrator changes can therefore change wider effective context
without the full-context fence. Paddock does not label the old candidate hash as
the immutable resulting effective policy. Runtime application status and returned
policy version/hash remain separate receipt fields. “Credential-free” describes
the proposed rule and observed review context; it does not promise that a trusted
admin can never later attach credentials affecting that destination.

The chunk status is updated separately after policy persistence. Concurrent edits,
rejection or refresh can race with that status/audit update. The exact rule applied
still comes from the captured chunk, but a later stored chunk view or an unrelated
administrator's decision is not a transactionally frozen record. Paddock's
single-winner claim covers Paddock callers, not all independent upstream admins.
These metadata races are not evidence that an untrusted proposal can substitute a
different rule into this captured approval.

If stronger guarantees are required, the upstream patch must hold the shared
sandbox mutation guard across evaluation/merge, validate the relevant live
provider/global/resource versions at persistence, and conditionally commit the
chunk decision against its captured fingerprint/status. A pending-only reject
primitive is separately required: native Reject can revoke already-approved
rules. This work is not needed to enable the narrower captured-rule contract and
has not been represented as implemented here.

## Verification and its limits

Paddock tests include:

- `TestAccessExactRuleProductionAdapterCanApproveAndReplay`: production
  constructor, structured Inspect, qualified actionable view, durable executor,
  original-token RPC approval, application receipt and no second grant on replay.
- `TestAccessExactRuleProductionAdapterRejectsChangedReview`: rule bytes, map key,
  token, sandbox identity and credential-authority changes cannot reach an
  approval RPC with the old frozen input.
- `TestAccessExactRuleProductionAdapterStaleTokenDoesNotRefreshOrGrant`: a
  precondition failure after inspection produces a durable stale receipt, keeps
  the saved token, and makes no replacement-token fetch or mutation retry.
- `TestOpenShellAccessApproveUsesOnlyCapturedTokenAndRequestUUID`: no token
  refresh; upstream precondition failures remain failures.
- Existing complete-rule, provider-overlap, unknown-field and cross-target tests.

These tests replace only the RPC connection with fixtures; no live grant occurs.
They test Paddock's transport/executor, not a substitute implementation of the
Rust server's token checker. Upstream source tests inspected include
`changed_policy_inputs_refresh_token_and_require_fresh_review` (policy.rs:17890),
`approval_refreshes_other_pending_proposals_for_sequential_review` (:18041),
`stale_refresh_does_not_revert_concurrent_proposal_edit` (:18244), and
`draft_chunk_handlers_reject_cross_sandbox_chunk_ids` (:21101). They were not
executed in this Go worktree. The captured-rule race conclusion is a source audit,
independently reviewed, with the explicit transaction limits stated above.
