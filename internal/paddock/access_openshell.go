package paddock

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	datamodel "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	api "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sandboxapi "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// OpenShellAccessBackend is the only network-rule mutation boundary used by the
// access executor. Approve consumes a previously captured token verbatim. It
// must never obtain a replacement token or invoke `openshell rule approve`.
type OpenShellAccessBackend interface {
	Inspect(context.Context, Task) (OpenShellAccessInspection, error)
	Approve(context.Context, Task, string, string, string) (OpenShellAccessApplication, error)
	Reject(context.Context, Task, string, string, string) error
}

type OpenShellAccessInspection struct {
	SandboxID    string
	Chunks       []OpenShellAccessChunk
	Application  OpenShellAccessApplication
	AdvisorState string
	AdvisorError string
	ApprovalMode string
	// ExactRuleBound means a nonempty captured token binds the applied rule/map
	// key and immutable sandbox. It does not fence independent admin context.
	ExactRuleBound    bool
	RejectPendingOnly bool
}

// Rule contains the entire structured rule in protobuf JSON using proto field
// names. Unknown protobuf fields are rejected, not silently omitted. ReviewToken
// is host-private and must never be included in a public access response.
type OpenShellAccessChunk struct {
	ID, Status, RuleName, Binary, Rationale, SecurityNotes   string
	ValidationResult, ApplicationError, ReviewToken          string
	CurrentEffectivePolicyHash, CandidateEffectivePolicyHash string
	SupersedesChunkID                                        string
	Rule                                                     json.RawMessage
	CurrentEffectivePolicy, CandidateEffectivePolicy         json.RawMessage
	Confidence                                               float64
	HitCount                                                 int32
	QualificationError                                       string
}

type OpenShellAccessApplication struct {
	Status        string
	PolicyVersion uint32
	PolicyHash    string
	Error         string
}

// OpenShellAccessBackendError deliberately excludes raw gRPC/status/config
// diagnostics: they may contain authentication or policy material. An ambiguous
// mutation is never safe to replay just because the CLI exits unsuccessfully.
type OpenShellAccessBackendError struct {
	Code             string
	Retryable        bool
	OutcomeUncertain bool
	message          string
}

func (e *OpenShellAccessBackendError) Error() string {
	if e.message != "" {
		return e.message
	}
	return "OpenShell access backend: " + e.Code
}

func accessBackendFailure(code, message string, retryable, uncertain bool) error {
	return &OpenShellAccessBackendError{Code: code, message: message, Retryable: retryable, OutcomeUncertain: uncertain}
}

type openShellAccessRPC interface {
	GetSandbox(context.Context, *api.GetSandboxRequest, ...grpc.CallOption) (*api.SandboxResponse, error)
	GetDraftPolicy(context.Context, *api.GetDraftPolicyRequest, ...grpc.CallOption) (*api.GetDraftPolicyResponse, error)
	GetSandboxConfig(context.Context, *sandboxapi.GetSandboxConfigRequest, ...grpc.CallOption) (*sandboxapi.GetSandboxConfigResponse, error)
	GetSandboxPolicyStatus(context.Context, *api.GetSandboxPolicyStatusRequest, ...grpc.CallOption) (*api.GetSandboxPolicyStatusResponse, error)
	ApproveDraftChunk(context.Context, *api.ApproveDraftChunkRequest, ...grpc.CallOption) (*api.ApproveDraftChunkResponse, error)
}

type openShellAccessBackend struct {
	// Tests replace only the connection factory, never the production grant path.
	connect func(OpenShellConfig) (openShellAccessRPC, func(), error)
}

func NewOpenShellAccessBackend() OpenShellAccessBackend {
	return &openShellAccessBackend{connect: connectOpenShellAccess}
}

func accessWorkspace(task Task) *datamodel.WorkspaceSelector {
	return &datamodel.WorkspaceSelector{Selection: &datamodel.WorkspaceSelector_Workspace{Workspace: task.OpenShell.Config.Workspace}}
}

func validateAccessTask(task Task) error {
	if task.RuntimeName() != "openshell" || task.OpenShell == nil || task.OpenShell.ID == "" || task.OpenShell.Name == "" || task.OpenShell.Config.Workspace == "" {
		return accessBackendFailure("target_unavailable", "A recorded OpenShell sandbox identity and workspace are required", false, false)
	}
	return nil
}

func verifyAccessSandbox(ctx context.Context, client openShellAccessRPC, task Task) error {
	response, err := client.GetSandbox(ctx, &api.GetSandboxRequest{Name: task.OpenShell.Name, WorkspaceScope: accessWorkspace(task)})
	if err != nil {
		return mapAccessRPCError(err, false)
	}
	meta := response.GetSandbox().GetMetadata()
	workspace := meta.GetWorkspace()
	if workspace == "" {
		workspace = "default" // ObjectMeta explicitly defines empty as default.
	}
	if meta.GetId() != task.OpenShell.ID || meta.GetName() != task.OpenShell.Name || workspace != task.OpenShell.Config.Workspace || meta.GetDeletionTime() != nil {
		return accessBackendFailure("target_replaced", "OpenShell sandbox identity no longer matches the task", false, false)
	}
	return nil
}

func (b *openShellAccessBackend) Inspect(ctx context.Context, task Task) (OpenShellAccessInspection, error) {
	result := OpenShellAccessInspection{Chunks: []OpenShellAccessChunk{}, AdvisorState: "unknown", ApprovalMode: "unknown", ExactRuleBound: true, Application: OpenShellAccessApplication{Status: "unknown"}}
	if err := validateAccessTask(task); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, closeConnection, err := b.connect(task.OpenShell.Config)
	if err != nil {
		return result, err
	}
	defer closeConnection()
	if err = verifyAccessSandbox(ctx, client, task); err != nil {
		return result, err
	}
	result.SandboxID = task.OpenShell.ID
	response, err := client.GetDraftPolicy(ctx, &api.GetDraftPolicyRequest{Sandbox: task.OpenShell.Name, WorkspaceScope: accessWorkspace(task)})
	if err != nil {
		return result, mapAccessRPCError(err, false)
	}
	if len(response.GetChunks()) > 1024 {
		return result, accessBackendFailure("backend_schema_unsupported", "OpenShell proposal response exceeds the supported bound", false, false)
	}
	for _, chunk := range response.GetChunks() {
		if chunk == nil || !validAccessID(chunk.GetId()) || chunk.GetProposedRule() == nil {
			return result, accessBackendFailure("backend_schema_unsupported", "OpenShell returned an incomplete structured proposal", false, false)
		}
		if hasUnknownAccessFields(chunk.ProtoReflect()) {
			return result, accessBackendFailure("backend_schema_unsupported", "OpenShell proposal contains unsupported fields; review the pinned adapter", false, false)
		}
		rule, err := marshalAccessProto(chunk.GetProposedRule().ProtoReflect())
		if err != nil {
			return result, err
		}
		current, err := marshalAccessProto(chunk.GetCurrentEffectivePolicy().ProtoReflect())
		if err != nil {
			return result, err
		}
		candidate, err := marshalAccessProto(chunk.GetCandidateEffectivePolicy().ProtoReflect())
		if err != nil {
			return result, err
		}
		result.Chunks = append(result.Chunks, OpenShellAccessChunk{
			ID: chunk.GetId(), Status: chunk.GetStatus(), RuleName: chunk.GetRuleName(), Binary: chunk.GetBinary(),
			Rationale: chunk.GetRationale(), SecurityNotes: chunk.GetSecurityNotes(), ValidationResult: chunk.GetValidationResult(),
			ApplicationError: chunk.GetApplicationError(), ReviewToken: chunk.GetReviewToken(),
			CurrentEffectivePolicyHash: chunk.GetCurrentEffectivePolicyHash(), CandidateEffectivePolicyHash: chunk.GetCandidateEffectivePolicyHash(),
			SupersedesChunkID: chunk.GetSupersedesChunkId(), Rule: rule, CurrentEffectivePolicy: current, CandidateEffectivePolicy: candidate,
			Confidence: float64(chunk.GetConfidence()), HitCount: chunk.GetHitCount(),
			QualificationError: qualifyExactNetworkRule(chunk),
		})
	}
	config, configErr := client.GetSandboxConfig(ctx, &sandboxapi.GetSandboxConfigRequest{Name: task.OpenShell.Name, WorkspaceScope: accessWorkspace(task)})
	if configErr != nil {
		result.AdvisorError = "Cannot inspect the effective policy-advisor setting"
	} else {
		result.AdvisorState = "unavailable"
		result.AdvisorError = "The policy advisor is disabled; an operator must enable it before agent-authored submission"
		if config.GetSettings()["agent_policy_proposals_enabled"].GetValue().GetBoolValue() {
			result.AdvisorState, result.AdvisorError = "available", ""
		}
		result.ApprovalMode = "manual"
		if value := config.GetSettings()["proposal_approval_mode"].GetValue().GetStringValue(); value != "" {
			result.ApprovalMode = value
		}
	}
	result.Application = inspectAccessApplication(ctx, client, task, 0, "")
	// v0.1.6 RejectDraftChunk accepts an already-approved rule and removes it.
	// A prior client-side read cannot prevent a race into revocation, so this
	// adapter never advertises pending-only rejection or invokes that RPC.
	result.RejectPendingOnly = false
	return result, nil
}

func (b *openShellAccessBackend) Approve(ctx context.Context, task Task, chunkID, reviewedToken, requestID string) (OpenShellAccessApplication, error) {
	result := OpenShellAccessApplication{Status: "unknown"}
	// The decision engine qualifies and seals the complete credential-free rule.
	// The gateway validates this captured token and merges that same captured
	// chunk into the immutable sandbox target. This transport does not promise a
	// transaction freezing separately administered provider/global policy.
	if err := validateAccessTask(task); err != nil {
		return result, err
	}
	if !validAccessID(chunkID) || !validAccessID(requestID) || strings.ReplaceAll(requestID, "-", "") == strings.Repeat("0", 32) || reviewedToken == "" || len(reviewedToken) > 4096 {
		return result, accessBackendFailure("invalid_request", "Approval requires a full chunk ID, durable request UUID, and captured review token", false, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, closeConnection, err := b.connect(task.OpenShell.Config)
	if err != nil {
		return result, err
	}
	defer closeConnection()
	if err = verifyAccessSandbox(ctx, client, task); err != nil {
		return result, err
	}
	response, err := client.ApproveDraftChunk(ctx, &api.ApproveDraftChunkRequest{
		Sandbox: task.OpenShell.Name, WorkspaceScope: accessWorkspace(task), ChunkId: chunkID, ReviewToken: reviewedToken, RequestId: requestID,
	})
	if err != nil {
		return result, mapAccessRPCError(err, true)
	}
	// An accepted merge is distinct from runtime load. Failed status inspection
	// does not turn a successful mutation into a safe-to-replay failure.
	return inspectAccessApplication(ctx, client, task, response.GetPolicyVersion(), response.GetPolicyHash()), nil
}

func (b *openShellAccessBackend) Reject(_ context.Context, _ Task, _, _, _ string) error {
	return accessBackendFailure("precondition_unavailable", "OpenShell 0.1.6 cannot atomically reject only a pending proposal; rejection is unavailable through this adapter", false, false)
}

func inspectAccessApplication(ctx context.Context, client openShellAccessRPC, task Task, version uint32, expectedHash string) OpenShellAccessApplication {
	result := OpenShellAccessApplication{Status: "unknown", PolicyVersion: version, PolicyHash: expectedHash}
	response, err := client.GetSandboxPolicyStatus(ctx, &api.GetSandboxPolicyStatusRequest{Sandbox: task.OpenShell.Name, WorkspaceScope: accessWorkspace(task), Version: version})
	if err != nil {
		result.Error = "OpenShell policy application status is unavailable"
		return result
	}
	revision := response.GetRevision()
	if revision == nil || (version != 0 && revision.GetVersion() != version) || (expectedHash != "" && revision.GetPolicyHash() != expectedHash) {
		result.Error = "OpenShell policy application evidence does not match the approved revision"
		return result
	}
	result.PolicyVersion, result.PolicyHash = revision.GetVersion(), revision.GetPolicyHash()
	switch revision.GetStatus() {
	case api.PolicyStatus_POLICY_STATUS_LOADED:
		if response.GetActiveVersion() == revision.GetVersion() {
			result.Status = "loaded"
		}
	case api.PolicyStatus_POLICY_STATUS_PENDING:
		result.Status = "pending"
	case api.PolicyStatus_POLICY_STATUS_FAILED:
		result.Status, result.Error = "error", "OpenShell runtime rejected the policy revision"
	case api.PolicyStatus_POLICY_STATUS_SUPERSEDED:
		result.Error = "The policy revision was superseded; current rule application requires inspection"
	}
	return result
}

func hasUnknownAccessFields(message protoreflect.Message) bool {
	if !message.IsValid() {
		return false
	}
	if len(message.GetUnknown()) != 0 {
		return true
	}
	found := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() && field.MapValue().Kind() == protoreflect.MessageKind {
			value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
				found = hasUnknownAccessFields(item.Message())
				return !found
			})
		} else if field.IsMap() && field.MapValue().Kind() == protoreflect.EnumKind {
			value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
				found = field.MapValue().Enum().Values().ByNumber(item.Enum()) == nil
				return !found
			})
		} else if field.IsList() && field.Kind() == protoreflect.MessageKind {
			items := value.List()
			for i := 0; i < items.Len() && !found; i++ {
				found = hasUnknownAccessFields(items.Get(i).Message())
			}
		} else if field.IsList() && field.Kind() == protoreflect.EnumKind {
			items := value.List()
			for i := 0; i < items.Len() && !found; i++ {
				found = field.Enum().Values().ByNumber(items.Get(i).Enum()) == nil
			}
		} else if field.Kind() == protoreflect.MessageKind && !field.IsMap() && !field.IsList() {
			found = hasUnknownAccessFields(value.Message())
		} else if field.Kind() == protoreflect.EnumKind && !field.IsMap() && !field.IsList() {
			found = field.Enum().Values().ByNumber(value.Enum()) == nil
		}
		return !found
	})
	return found
}

func marshalAccessProto(message protoreflect.Message) (json.RawMessage, error) {
	if !message.IsValid() {
		return json.RawMessage("null"), nil
	}
	data, err := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(message.Interface())
	if err != nil || len(data) > 2<<20 {
		return nil, accessBackendFailure("backend_schema_unsupported", "OpenShell structured policy exceeds supported schema or size bounds", false, false)
	}
	return data, nil
}

func mapAccessRPCError(err error, mutation bool) error {
	code := status.Code(err)
	switch code {
	case codes.FailedPrecondition:
		return accessBackendFailure("proposal_changed", "OpenShell rejected the reviewed precondition; inspect and review again", false, false)
	case codes.PermissionDenied, codes.Unauthenticated:
		return accessBackendFailure("unauthorized", "OpenShell authentication or authorization is unavailable", false, false)
	case codes.NotFound:
		return accessBackendFailure("target_unavailable", "OpenShell sandbox or proposal is unavailable", false, false)
	case codes.InvalidArgument:
		return accessBackendFailure("invalid_request", "OpenShell rejected the access operation input", false, false)
	case codes.Unimplemented:
		return accessBackendFailure("precondition_unavailable", "The gateway does not implement the qualified access API", false, false)
	default:
		if mutation {
			return accessBackendFailure("outcome_uncertain", "OpenShell decision outcome is uncertain; reconcile before any retry", false, true)
		}
		return accessBackendFailure("backend_unavailable", "OpenShell access inspection is unavailable", true, false)
	}
}

type openShellAccessMetadata struct {
	Name     string `json:"name"`
	Endpoint string `json:"gateway_endpoint"`
	AuthMode string `json:"auth_mode"`
}

// resolveAccessGateway mirrors the pinned CLI's named gateway and user mTLS
// layout. It never consults active_gateway, changes configuration, refreshes
// tokens, or falls back to ambient credentials. Bearer, OIDC, plaintext, and
// endpoint-path/tunnel transports are intentionally unavailable in this version.
func resolveAccessGateway(config OpenShellConfig) (string, *tls.Config, error) {
	failure := func() (string, *tls.Config, error) {
		return "", nil, accessBackendFailure("backend_unavailable", "Structured access requires a configured HTTPS gateway with existing CLI mTLS materials", false, false)
	}
	if config.Gateway == "" || config.Gateway == "." || config.Gateway == ".." || strings.ContainsAny(config.Gateway, "/\\\x00") || filepath.Base(config.Gateway) != config.Gateway {
		return failure()
	}
	configHome := config.ConfigHome
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return failure()
		}
		configHome = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configHome) {
		return failure()
	}
	userGateway := filepath.Join(configHome, "openshell", "gateways", config.Gateway)
	metadataPath := filepath.Join(userGateway, "metadata.json")
	metadataBytes, err := readAccessRegularFile(metadataPath, 64<<10)
	if os.IsNotExist(err) {
		// c.env() does not forward OPENSHELL_SYSTEM_GATEWAY_DIR; use the same
		// default system registry instead of inheriting a mutable environment.
		metadataBytes, err = readAccessRegularFile(filepath.Join("/etc/openshell/gateways", config.Gateway, "metadata.json"), 64<<10)
	}
	if err != nil {
		return failure()
	}
	var metadata openShellAccessMetadata
	if json.Unmarshal(metadataBytes, &metadata) != nil || metadata.Name != config.Gateway || (metadata.AuthMode != "" && metadata.AuthMode != "mtls") {
		return failure()
	}
	u, err := url.Parse(metadata.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return failure()
	}
	materials := filepath.Join(userGateway, "mtls")
	// OPENSHELL_LOCAL_TLS_DIR is a local-gateway bootstrap override, not an
	// alternative CLI client-certificate layout in the pinned TLS resolver.
	// Refuse a mismatched override rather than silently selecting credentials.
	if config.TLSDir != "" && filepath.Clean(config.TLSDir) != materials {
		return failure()
	}
	ca, err := readAccessRegularFile(filepath.Join(materials, "ca.crt"), 1<<20)
	if err != nil {
		return failure()
	}
	cert, err := readAccessRegularFile(filepath.Join(materials, "tls.crt"), 1<<20)
	if err != nil {
		return failure()
	}
	key, err := readAccessRegularFile(filepath.Join(materials, "tls.key"), 1<<20)
	if err != nil {
		return failure()
	}
	identity, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return failure()
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return failure()
	}
	return u.Host, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{identity}, ServerName: u.Hostname()}, nil
}

func readAccessRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("unsupported access configuration file")
	}
	f, err := openAccessConfigFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return nil, fmt.Errorf("unsupported access configuration file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("unsupported access configuration file")
	}
	return data, nil
}

func connectOpenShellAccess(config OpenShellConfig) (openShellAccessRPC, func(), error) {
	address, tlsConfig, err := resolveAccessGateway(config)
	if err != nil {
		return nil, nil, err
	}
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithDisableRetry(), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20)))
	if err != nil {
		return nil, nil, accessBackendFailure("backend_unavailable", "OpenShell structured access connection is unavailable", true, false)
	}
	return api.NewOpenShellClient(connection), func() { _ = connection.Close() }, nil
}
