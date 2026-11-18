package paddock

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	datamodel "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	api "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sandboxapi "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const structuredAccessChunkID = "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89"
const structuredAccessRequestID = "8c47f4cb-4808-4ae4-b0d2-f0e5c4937592"

type structuredAccessRPCFixture struct {
	task           Task
	chunk          *api.PolicyChunk
	approveRequest *api.ApproveDraftChunkRequest
	approveCalls   int
	approveErr     error
	inspectErr     error
	configErr      error
	applicationErr error
	policyStatus   api.PolicyStatus
	activeVersion  uint32
	draftReads     int
	metadataID     string
}

func (f *structuredAccessRPCFixture) GetSandbox(_ context.Context, request *api.GetSandboxRequest, _ ...grpc.CallOption) (*api.SandboxResponse, error) {
	if request.GetName() != f.task.OpenShell.Name || request.GetWorkspaceScope().GetWorkspace() != f.task.OpenShell.Config.Workspace {
		return nil, status.Error(codes.InvalidArgument, "wrong target")
	}
	return &api.SandboxResponse{Sandbox: &api.Sandbox{Metadata: &datamodel.ObjectMeta{Id: f.metadataID, Name: f.task.OpenShell.Name, Workspace: f.task.OpenShell.Config.Workspace}}}, nil
}

func (f *structuredAccessRPCFixture) GetDraftPolicy(_ context.Context, request *api.GetDraftPolicyRequest, _ ...grpc.CallOption) (*api.GetDraftPolicyResponse, error) {
	f.draftReads++
	if request.GetSandbox() != f.task.OpenShell.Name || request.GetWorkspaceScope().GetWorkspace() != f.task.OpenShell.Config.Workspace || request.GetStatusFilter() != "" {
		return nil, status.Error(codes.InvalidArgument, "wrong target or filter")
	}
	return &api.GetDraftPolicyResponse{Chunks: []*api.PolicyChunk{f.chunk}}, f.inspectErr
}

func (f *structuredAccessRPCFixture) GetSandboxConfig(_ context.Context, _ *sandboxapi.GetSandboxConfigRequest, _ ...grpc.CallOption) (*sandboxapi.GetSandboxConfigResponse, error) {
	return &sandboxapi.GetSandboxConfigResponse{Settings: map[string]*sandboxapi.EffectiveSetting{
		"agent_policy_proposals_enabled": {Value: &sandboxapi.SettingValue{Value: &sandboxapi.SettingValue_BoolValue{BoolValue: true}}},
		"proposal_approval_mode":         {Value: &sandboxapi.SettingValue{Value: &sandboxapi.SettingValue_StringValue{StringValue: "manual"}}},
	}}, f.configErr
}

func (f *structuredAccessRPCFixture) GetSandboxPolicyStatus(_ context.Context, request *api.GetSandboxPolicyStatusRequest, _ ...grpc.CallOption) (*api.GetSandboxPolicyStatusResponse, error) {
	version := request.GetVersion()
	if version == 0 {
		version = 7
	}
	return &api.GetSandboxPolicyStatusResponse{Revision: &api.SandboxPolicyRevision{Version: version, PolicyHash: "stored-policy-revision-hash", Status: f.policyStatus}, ActiveVersion: f.activeVersion}, f.applicationErr
}

func (f *structuredAccessRPCFixture) ApproveDraftChunk(_ context.Context, request *api.ApproveDraftChunkRequest, _ ...grpc.CallOption) (*api.ApproveDraftChunkResponse, error) {
	f.approveCalls++
	f.approveRequest = proto.Clone(request).(*api.ApproveDraftChunkRequest)
	return &api.ApproveDraftChunkResponse{PolicyVersion: 7, PolicyHash: "stored-policy-revision-hash"}, f.approveErr
}

func structuredAccessFixture(t *testing.T) (*openShellAccessBackend, *structuredAccessRPCFixture, Task) {
	t.Helper()
	task := accessTaskFixture()
	task.OpenShell.ID = "immutable-sandbox-id"
	rule := &sandboxapi.NetworkPolicyRule{}
	if err := protojson.Unmarshal([]byte(`{"name":"restricted","endpoints":[{"host":"registry.example","ports":[443,8443],"protocol":"rest","path":"/packages/**","rules":[{"allow":{"method":"GET","path":"/packages/**"}}],"allowed_ips":["203.0.113.0/24"],"allow_encoded_slash":true,"request_body_credential_rewrite":true}],"binaries":[{"path":"/usr/bin/npm"}]}`), rule); err != nil {
		t.Fatal(err)
	}
	fake := &structuredAccessRPCFixture{task: task, metadataID: task.OpenShell.ID, policyStatus: api.PolicyStatus_POLICY_STATUS_LOADED, activeVersion: 7, chunk: &api.PolicyChunk{
		Id: structuredAccessChunkID, Status: "pending", RuleName: "restricted", ProposedRule: rule, ReviewToken: "stored-review-token",
		Rationale: "dependency download", SecurityNotes: "credential reach", ValidationResult: "review complete", Binary: "/usr/bin/npm",
		CurrentEffectivePolicyHash: "complete-current-hash", CandidateEffectivePolicyHash: "complete-candidate-hash", HitCount: 4,
		CurrentEffectivePolicy: &sandboxapi.SandboxPolicy{}, CandidateEffectivePolicy: &sandboxapi.SandboxPolicy{},
	}}
	backend := &openShellAccessBackend{connect: func(config OpenShellConfig) (openShellAccessRPC, func(), error) {
		if config.Gateway != task.OpenShell.Config.Gateway || config.Workspace != task.OpenShell.Config.Workspace {
			t.Fatal("connection configuration changed")
		}
		return fake, func() {}, nil
	}}
	return backend, fake, task
}

func TestOpenShellAccessStockTransportQualifiesEachRule(t *testing.T) {
	_, fake, task := structuredAccessFixture(t)
	backend := NewOpenShellAccessBackend().(*openShellAccessBackend)
	backend.connect = func(OpenShellConfig) (openShellAccessRPC, func(), error) { return fake, func() {}, nil }
	inspection, err := backend.Inspect(context.Background(), task)
	if err != nil || !inspection.ExactRuleBound || inspection.RejectPendingOnly || inspection.Chunks[0].QualificationError == "" {
		t.Fatal("credentialed fixture unexpectedly qualified", inspection, err)
	}
	setSafeExactRuleFixture(t, fake)
	inspection, err = backend.Inspect(context.Background(), task)
	if err != nil || !inspection.ExactRuleBound || inspection.Chunks[0].QualificationError != "" {
		t.Fatal("safe npm rule did not qualify", inspection, err)
	}
}

func TestOpenShellAccessInspectPreservesCompleteRuleAndFindings(t *testing.T) {
	backend, fake, task := structuredAccessFixture(t)
	result, err := backend.Inspect(context.Background(), task)
	if err != nil || result.SandboxID != task.OpenShell.ID || len(result.Chunks) != 1 {
		t.Fatal(result, err)
	}
	chunk := result.Chunks[0]
	var actualRule sandboxapi.NetworkPolicyRule
	if err := protojson.Unmarshal(chunk.Rule, &actualRule); err != nil || !proto.Equal(&actualRule, fake.chunk.ProposedRule) {
		t.Fatalf("lost permission-relevant rule content: %s %v", chunk.Rule, err)
	}
	if chunk.ReviewToken != "stored-review-token" || chunk.CandidateEffectivePolicyHash != "complete-candidate-hash" || chunk.SecurityNotes != "credential reach" || result.Application.Status != "loaded" || result.AdvisorState != "available" || result.ApprovalMode != "manual" {
		t.Fatal(result)
	}
	if result.RejectPendingOnly || fake.approveRequest != nil {
		t.Fatal("inspection advertised unsafe rejection or mutated access")
	}
}

func TestOpenShellAccessApproveUsesOnlyCapturedTokenAndRequestUUID(t *testing.T) {
	backend, fake, task := structuredAccessFixture(t)
	// A fresh proposal lookup would see this replacement token. Approve must
	// instead send the old token, leaving the atomic precondition to upstream.
	fake.chunk.ReviewToken = "replacement-token-that-must-not-be-used"
	fake.approveErr = status.Error(codes.FailedPrecondition, "credential-containing diagnostic must not escape")
	_, err := backend.Approve(context.Background(), task, structuredAccessChunkID, "old-reviewed-token", structuredAccessRequestID)
	var backendErr *OpenShellAccessBackendError
	if !errors.As(err, &backendErr) || backendErr.Code != "proposal_changed" || backendErr.OutcomeUncertain || strings.Contains(err.Error(), "credential-containing") {
		t.Fatal(err)
	}
	request := fake.approveRequest
	if fake.draftReads != 0 || request.GetReviewToken() != "old-reviewed-token" || request.GetRequestId() != structuredAccessRequestID || request.GetChunkId() != structuredAccessChunkID || request.GetSandbox() != task.OpenShell.Name || request.GetWorkspaceScope().GetWorkspace() != task.OpenShell.Config.Workspace {
		t.Fatal("approval refreshed its token or changed its target", request, fake.draftReads)
	}
}

func TestOpenShellAccessApprovalReceiptSurvivesUnknownApplication(t *testing.T) {
	backend, fake, task := structuredAccessFixture(t)
	fake.applicationErr = status.Error(codes.Unavailable, "private upstream text")
	result, err := backend.Approve(context.Background(), task, structuredAccessChunkID, "stored-token", structuredAccessRequestID)
	if err != nil || result.Status != "unknown" || result.PolicyVersion != 7 || result.PolicyHash != "stored-policy-revision-hash" || strings.Contains(result.Error, "private") {
		t.Fatal(result, err)
	}
	fake.applicationErr = nil
	fake.policyStatus, fake.activeVersion = api.PolicyStatus_POLICY_STATUS_PENDING, 6
	result, err = backend.Approve(context.Background(), task, structuredAccessChunkID, "stored-token", structuredAccessRequestID)
	if err != nil || result.Status != "pending" {
		t.Fatal(result, err)
	}
	fake.policyStatus, fake.activeVersion = api.PolicyStatus_POLICY_STATUS_LOADED, 8
	result, err = backend.Approve(context.Background(), task, structuredAccessChunkID, "stored-token", structuredAccessRequestID)
	if err != nil || result.Status != "unknown" {
		t.Fatal("newer active policy was reported as this revision loaded", result, err)
	}
}

func TestOpenShellAccessRejectNeverInvokesUnsafeUpstream(t *testing.T) {
	backend, fake, task := structuredAccessFixture(t)
	err := backend.Reject(context.Background(), task, structuredAccessChunkID, structuredAccessRequestID, "not required")
	var backendErr *OpenShellAccessBackendError
	if !errors.As(err, &backendErr) || backendErr.Code != "precondition_unavailable" || fake.draftReads != 0 || fake.approveRequest != nil {
		t.Fatal(err)
	}
}

func TestOpenShellAccessRejectsReplacedIdentityAndUnknownRuleFields(t *testing.T) {
	backend, fake, task := structuredAccessFixture(t)
	fake.metadataID = "new-sandbox-with-reused-name"
	_, err := backend.Inspect(context.Background(), task)
	var backendErr *OpenShellAccessBackendError
	if !errors.As(err, &backendErr) || backendErr.Code != "target_replaced" || fake.draftReads != 0 {
		t.Fatal(err)
	}
	fake.metadataID = task.OpenShell.ID
	// Unknown field 100 contains data an older adapter could otherwise drop.
	fake.chunk.ProposedRule.Endpoints[0].ProtoReflect().SetUnknown([]byte{0xa2, 0x06, 0x01, 0x78})
	_, err = backend.Inspect(context.Background(), task)
	if !errors.As(err, &backendErr) || backendErr.Code != "backend_schema_unsupported" {
		t.Fatal("unknown permission scope was silently dropped", err)
	}
	fake.chunk.ProposedRule.Endpoints[0].ProtoReflect().SetUnknown(nil)
	fake.chunk.ProposedRule.Endpoints[0].Enforcement = sandboxapi.NetworkEnforcementMode(999)
	_, err = backend.Inspect(context.Background(), task)
	if !errors.As(err, &backendErr) || backendErr.Code != "backend_schema_unsupported" {
		t.Fatal("unknown permission enum was silently accepted", err)
	}
}

func TestOpenShellAccessAmbiguousMutationIsNotRetryable(t *testing.T) {
	backend, fake, task := structuredAccessFixture(t)
	fake.approveErr = status.Error(codes.DeadlineExceeded, "private error")
	_, err := backend.Approve(context.Background(), task, structuredAccessChunkID, "stored-token", structuredAccessRequestID)
	var backendErr *OpenShellAccessBackendError
	if !errors.As(err, &backendErr) || backendErr.Code != "outcome_uncertain" || !backendErr.OutcomeUncertain || backendErr.Retryable {
		t.Fatal(err)
	}
}

func accessMTLSFixture(t *testing.T) OpenShellConfig {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := OpenShellConfig{ConfigHome: t.TempDir(), Gateway: "fixture", Workspace: "default"}
	dir := filepath.Join(config.ConfigHome, "openshell", "gateways", config.Gateway)
	if err := os.MkdirAll(filepath.Join(dir, "mtls"), 0700); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(openShellAccessMetadata{Name: "fixture", Endpoint: "https://gateway.example:8080", AuthMode: "mtls"})
	files := map[string][]byte{
		"metadata.json": metadata,
		"mtls/ca.crt":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		"mtls/tls.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		"mtls/tls.key":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return config
}

func TestOpenShellAccessGatewayUsesPinnedVerifiedMTLSOnly(t *testing.T) {
	config := accessMTLSFixture(t)
	t.Setenv("XDG_CONFIG_HOME", "/do-not-use-ambient-config")
	t.Setenv("OPENSHELL_GATEWAY", "do-not-use-active-gateway")
	address, tlsConfig, err := resolveAccessGateway(config)
	if err != nil || address != "gateway.example:8080" || tlsConfig.ServerName != "gateway.example" || tlsConfig.InsecureSkipVerify || tlsConfig.RootCAs == nil || len(tlsConfig.Certificates) != 1 {
		t.Fatal(address, tlsConfig, err)
	}
	metadataPath := filepath.Join(config.ConfigHome, "openshell", "gateways", "fixture", "metadata.json")
	for _, metadata := range []openShellAccessMetadata{
		{Name: "fixture", Endpoint: "http://gateway.example:8080", AuthMode: "plaintext"},
		{Name: "fixture", Endpoint: "https://gateway.example:8080", AuthMode: "oidc"},
		{Name: "fixture", Endpoint: "https://credential:secret@gateway.example:8080", AuthMode: "mtls"},
		{Name: "fixture", Endpoint: "https://gateway.example/path", AuthMode: "mtls"},
		{Name: "wrong-gateway", Endpoint: "https://gateway.example:8080", AuthMode: "mtls"},
	} {
		data, _ := json.Marshal(metadata)
		if err := os.WriteFile(metadataPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := resolveAccessGateway(config)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe gateway configuration was accepted or leaked", err)
		}
	}
}
