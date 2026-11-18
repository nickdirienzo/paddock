package paddock

import (
	"context"
	"slices"
	"testing"

	api "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sandboxapi "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func setSafeExactRuleFixture(t *testing.T, f *structuredAccessRPCFixture) {
	t.Helper()
	rule := &sandboxapi.NetworkPolicyRule{}
	if err := protojson.Unmarshal([]byte(`{"name":"restricted","endpoints":[{"host":"registry.npmjs.org","port":443,"protocol":"rest","enforcement":1,"access":1}],"binaries":[{"path":"/usr/local/bin/node"}]}`), rule); err != nil {
		t.Fatal(err)
	}
	f.chunk.ProposedRule = rule
	f.chunk.SecurityNotes = ""
	f.chunk.ValidationResult = "no new findings"
	f.chunk.CandidateEffectivePolicy = &sandboxapi.SandboxPolicy{NetworkPolicies: map[string]*sandboxapi.NetworkPolicyRule{"restricted": proto.Clone(rule).(*sandboxapi.NetworkPolicyRule)}}
}

func TestOpenShellAccessExactRuleRejectsAuthority(t *testing.T) {
	cases := map[string]func(*api.PolicyChunk){
		"binding": func(c *api.PolicyChunk) {
			c.ProposedRule.Endpoints[0].CredentialBinding = &sandboxapi.NetworkCredentialBinding{Provider: "secret"}
		},
		"signing":                 func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].CredentialSigning = "sigv4" },
		"signing service":         func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].SigningService = "example-service" },
		"signing region":          func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].SigningRegion = "us-east-1" },
		"rewrite":                 func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].RequestBodyCredentialRewrite = true },
		"websocket rewrite":       func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].WebsocketCredentialRewrite = true },
		"uninspected credentials": func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].AllowUninspectedCredentials = true },
		"provider marker":         func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].ProviderCredentialed = true },
		"token owner":             func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].TokenGrantOwner = "authority" },
		"reserved name":           func(c *api.PolicyChunk) { c.RuleName = "_provider_secret"; c.ProposedRule.Name = c.RuleName },
		"hidden map name":         func(c *api.PolicyChunk) { c.RuleName = "another_name" },
		"tls bypass": func(c *api.PolicyChunk) {
			c.ProposedRule.Endpoints[0].Tls = sandboxapi.NetworkTlsMode_NETWORK_TLS_MODE_SKIP
		},
		"non REST": func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].Protocol = "tcp" },
		"audit only": func(c *api.PolicyChunk) {
			c.ProposedRule.Endpoints[0].Enforcement = sandboxapi.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_AUDIT
		},
		"wildcard host": func(c *api.PolicyChunk) { c.ProposedRule.Endpoints[0].Host = "*.npmjs.org" },
		"unknown field": func(c *api.PolicyChunk) {
			c.ProposedRule.Endpoints[0].ProtoReflect().SetUnknown([]byte{0xa2, 0x06, 0x01, 0x78})
		},
		"unknown rule field": func(c *api.PolicyChunk) {
			c.ProposedRule.ProtoReflect().SetUnknown([]byte{0xa2, 0x06, 0x01, 0x78})
		},
		"unknown binary field": func(c *api.PolicyChunk) {
			c.ProposedRule.Binaries[0].ProtoReflect().SetUnknown([]byte{0xa2, 0x06, 0x01, 0x78})
		},
		"unknown chunk field": func(c *api.PolicyChunk) {
			c.ProtoReflect().SetUnknown([]byte{0xa2, 0x06, 0x01, 0x78})
		},
		"provider overlap": func(c *api.PolicyChunk) {
			c.CandidateEffectivePolicy.NetworkPolicies["_provider_secret"] = &sandboxapi.NetworkPolicyRule{Endpoints: []*sandboxapi.NetworkEndpoint{{Host: "*.npmjs.org", Port: 443, ProviderCredentialed: true}}}
		},
	}
	cases["provider globstar zero labels"] = func(c *api.PolicyChunk) {
		c.ProposedRule.Endpoints[0].Host = "npmjs.org"
		c.CandidateEffectivePolicy.NetworkPolicies["_provider_secret"] = &sandboxapi.NetworkPolicyRule{Endpoints: []*sandboxapi.NetworkEndpoint{{Host: "**.npmjs.org", Port: 443, ProviderCredentialed: true}}}
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, f, _ := structuredAccessFixture(t)
			setSafeExactRuleFixture(t, f)
			mutate(f.chunk)
			if qualifyExactNetworkRule(f.chunk) == "" {
				t.Fatal("unsafe rule qualified")
			}
		})
	}
	_, f, _ := structuredAccessFixture(t)
	setSafeExactRuleFixture(t, f)
	f.chunk.CandidateEffectivePolicy.NetworkPolicies["_provider_other"] = &sandboxapi.NetworkPolicyRule{Endpoints: []*sandboxapi.NetworkEndpoint{{Host: "api.anthropic.com", Port: 443, ProviderCredentialed: true}}}
	if got := qualifyExactNetworkRule(f.chunk); got != "" {
		t.Fatal("unrelated attached credential provider blocked package downloads", got)
	}
}

func TestAccessExactRuleQualificationAndMapNameAreSealed(t *testing.T) {
	s, task, b, id := accessReviewFixture(t)
	view := s.ShowAccessV1(context.Background(), task.ID, id, true)
	if view.Error != nil || view.Request.DecisionSemantics.Approve != "exact_reviewed_rule" || view.Request.Snapshot.RuleName != "restricted" {
		t.Fatal(view)
	}
	// The policy map key is separate from the rule protobuf. Changing only that
	// immutable scope field must change the revision even with identical bytes.
	changed := *view.Request.Snapshot
	changed.RuleName = "different_map_key"
	digest, err := accessDigest(changed)
	if err != nil || digest == view.Request.Revision {
		t.Fatal("rule map key was not included in immutable revision", digest, err)
	}
	input := accessReviewInput(t, s, task, id)
	b.inspection.Chunks[0].QualificationError = "Credential authority cannot be reviewed through this path."
	unavailable := s.ShowAccessV1(context.Background(), task.ID, id, true)
	if unavailable.Error != nil || unavailable.Request.Actionable || unavailable.Request.Error.Code != "rule_scope_unsupported" {
		t.Fatal(unavailable)
	}
	if got := s.DecideAccessV1(context.Background(), id, input); got.Error == nil || b.approveCalls != 0 {
		t.Fatal("unqualified rule executed", got)
	}
}

func exactRuleProductionAdapterFixture(t *testing.T) (Service, Task, *structuredAccessRPCFixture) {
	t.Helper()
	_, rpc, task := structuredAccessFixture(t)
	setSafeExactRuleFixture(t, rpc)
	// Use the production constructor/Inspect/Approve path. Replace only the RPC
	// connection with the pinned structured fixture; no live grants are made.
	backend := NewOpenShellAccessBackend().(*openShellAccessBackend)
	backend.connect = func(OpenShellConfig) (openShellAccessRPC, func(), error) { return rpc, func() {}, nil }
	s := Service{Store: openTestStore(t, t.TempDir()), accessBackend: backend}
	if err := s.Store.Insert(task); err != nil {
		t.Fatal(err)
	}
	return s, task, rpc
}

func TestAccessExactRuleProductionAdapterCanApproveAndReplay(t *testing.T) {
	s, task, rpc := exactRuleProductionAdapterFixture(t)
	show := s.ShowAccessV1(context.Background(), task.ID, structuredAccessChunkID, true)
	if show.Error != nil || show.Request == nil || !show.Request.Actionable || !accessAllows(*show.Request, "approve") || accessAllows(*show.Request, "reject") || show.Request.DecisionSemantics.Approve != "exact_reviewed_rule" || !slices.Contains(show.Features, "network_review_token") {
		t.Fatal("production adapter did not expose a usable qualified action", show)
	}
	var disclosed sandboxapi.NetworkPolicyRule
	if err := protojson.Unmarshal(show.Request.Snapshot.Rule, &disclosed); err != nil || !proto.Equal(&disclosed, rpc.chunk.ProposedRule) || show.Request.Snapshot.RuleName != rpc.chunk.RuleName {
		t.Fatal("public snapshot did not disclose the complete endpoint/binary rule and map key", show, err)
	}
	in := AccessDecisionInput{ProtocolVersion: 1, TaskID: task.ID, Revision: show.Request.Revision, Decision: "approve", ExternalID: "production-path-action", Actor: AccessActor{Source: "arnold-slack", WorkspaceID: "T1", UserID: "U1"}, Reason: "Download required package metadata"}
	got := s.DecideAccessV1(context.Background(), structuredAccessChunkID, in)
	if got.Error != nil || got.Receipt == nil || got.Receipt.Decision.State != "confirmed" || got.Receipt.Grant.Status != "approved" || got.Receipt.Grant.ApplicationStatus != "loaded" || rpc.approveCalls != 1 {
		t.Fatal("qualified action could not execute through production adapter", got, rpc.approveCalls)
	}
	// The evaluated effective candidate and returned stored policy revision
	// describe different objects. Exact-rule approval must not demand equality
	// or relabel the evaluated candidate as the resulting full effective policy.
	if show.Request.Snapshot.CandidateEffectivePolicyHash != "complete-candidate-hash" || got.Receipt.Grant.PolicyHash != "stored-policy-revision-hash" || show.Request.Snapshot.CandidateEffectivePolicyHash == got.Receipt.Grant.PolicyHash {
		t.Fatal("evaluated context was conflated with returned revision", show, got)
	}
	if rpc.approveRequest.GetReviewToken() != "stored-review-token" || rpc.approveRequest.GetChunkId() != structuredAccessChunkID || !validAccessID(rpc.approveRequest.GetRequestId()) {
		t.Fatal("production action changed reviewed identity/token", rpc.approveRequest)
	}
	replay := s.DecideAccessV1(context.Background(), structuredAccessChunkID, in)
	if replay.Error != nil || replay.Receipt == nil || replay.Receipt.ID != got.Receipt.ID || rpc.approveCalls != 1 {
		t.Fatal("idempotent replay repeated the grant", replay, rpc.approveCalls)
	}
}

func TestAccessExactRuleProductionAdapterRejectsChangedReview(t *testing.T) {
	for _, change := range []string{"rule bytes", "map key", "review token", "sandbox identity", "credential authority"} {
		t.Run(change, func(t *testing.T) {
			s, task, rpc := exactRuleProductionAdapterFixture(t)
			in := accessReviewInput(t, s, task, structuredAccessChunkID)
			switch change {
			case "rule bytes":
				rpc.chunk.ProposedRule.Endpoints[0].Host = "different.example"
			case "map key":
				rpc.chunk.RuleName, rpc.chunk.ProposedRule.Name = "different", "different"
			case "review token":
				rpc.chunk.ReviewToken = "replacement-token"
			case "sandbox identity":
				rpc.metadataID = "replacement-sandbox"
			case "credential authority":
				rpc.chunk.ProposedRule.Endpoints[0].CredentialBinding = &sandboxapi.NetworkCredentialBinding{Provider: "secret"}
			}
			got := s.DecideAccessV1(context.Background(), structuredAccessChunkID, in)
			if got.Error == nil || rpc.approveCalls != 0 {
				t.Fatal("changed review reached an approval RPC", change, got, rpc.approveCalls)
			}
		})
	}
}

func TestAccessExactRuleProductionAdapterStaleTokenDoesNotRefreshOrGrant(t *testing.T) {
	s, task, rpc := exactRuleProductionAdapterFixture(t)
	in := accessReviewInput(t, s, task, structuredAccessChunkID)
	reads := rpc.draftReads
	// Model an upstream change after the last inspection but before approval.
	// The real gateway rejects that captured token; the adapter must surface the
	// failed precondition instead of fetching a replacement and granting it.
	rpc.approveErr = status.Error(codes.FailedPrecondition, "private upstream diagnostic")
	got := s.DecideAccessV1(context.Background(), structuredAccessChunkID, in)
	if got.Error == nil || got.Error.Code != "proposal_changed" || got.Receipt == nil || got.Receipt.Decision.State != "stale" || got.Receipt.Grant.Status == "approved" {
		t.Fatal("stale upstream token was accepted or lost its durable receipt", got)
	}
	if rpc.approveCalls != 1 || rpc.approveRequest.GetReviewToken() != "stored-review-token" || rpc.draftReads != reads+1 {
		t.Fatal("stale approval silently refreshed/retried its token", rpc.approveCalls, rpc.draftReads, rpc.approveRequest)
	}
}
