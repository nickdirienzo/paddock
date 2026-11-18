package paddock

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const AccessProtocolVersion = 1
const accessNotificationCaller = "paddock:access-notification"
const maxAccessInput = 64 << 10

var accessFeatures = []string{"task_bound_requests", "immutable_revision", "decision_idempotency", "single_winner", "network_review_token", "notification_receipts", "integration_results"}

// AccessError is safe for integration output. Backend transport/credential errors
// must not be embedded in Message.
type AccessError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *AccessError) Error() string { return e.Code + ": " + e.Message }
func accessError(code, message string, retryable bool) *AccessError {
	return &AccessError{code, message, retryable}
}

type AccessActor struct {
	Source      string `json:"source"`
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
}
type AccessTarget struct {
	SandboxID   string `json:"sandbox_id"`
	SandboxName string `json:"sandbox_name"`
	Gateway     string `json:"gateway"`
	Workspace   string `json:"workspace"`
}
type AccessRefresh struct {
	State     string       `json:"state"`
	CheckedAt string       `json:"checked_at"`
	Error     *AccessError `json:"error"`
}
type AccessAdvisor struct {
	State string       `json:"state"`
	Error *AccessError `json:"error"`
}

// AccessSnapshot is immutable. It contains the COMPLETE rule, not a formatted
// projection. Only a hash of the review token is public; the token stays in the
// host-owned snapshot record. Volatile hit counters/timestamps are excluded.
type AccessSnapshot struct {
	SchemaVersion int             `json:"schema_version"`
	TaskID        string          `json:"task_id"`
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	Target        AccessTarget    `json:"target"`
	Rule          json.RawMessage `json:"rule"`
	// RuleName is the policy map key, disclosed and hashed independently of the
	// protobuf's rule.name. Required for every qualified network approval.
	RuleName                     string   `json:"rule_name,omitempty"`
	Rationale                    string   `json:"rationale"`
	Security                     []string `json:"security"`
	ValidationResult             string   `json:"validation_result"`
	ApplicationError             string   `json:"application_error"`
	CurrentEffectivePolicyHash   string   `json:"current_effective_policy_hash"`
	CandidateEffectivePolicyHash string   `json:"candidate_effective_policy_hash"`
	ReviewTokenDigest            string   `json:"review_token_digest"`
}

type AccessDecisionInput struct {
	ProtocolVersion int         `json:"protocol_version"`
	TaskID          string      `json:"task_id"`
	Revision        string      `json:"revision"`
	Decision        string      `json:"decision"`
	ExternalID      string      `json:"external_id"`
	Actor           AccessActor `json:"actor"`
	Reason          string      `json:"reason"`
}
type AccessDecisionState struct {
	Value     string       `json:"value"`
	Caller    string       `json:"caller"`
	Actor     AccessActor  `json:"actor"`
	Reason    string       `json:"reason"`
	State     string       `json:"state"`
	CreatedAt string       `json:"created_at"`
	UpdatedAt string       `json:"updated_at"`
	Error     *AccessError `json:"error"`
}
type AccessGrant struct {
	Status            string       `json:"status"`
	ApplicationStatus string       `json:"application_status"`
	PolicyVersion     uint32       `json:"policy_version"`
	PolicyHash        string       `json:"policy_hash"`
	Error             *AccessError `json:"error"`
}
type AccessNotification struct {
	Status          string       `json:"status"`
	ExternalID      string       `json:"external_id"`
	CallerNamespace string       `json:"caller_namespace"`
	ThreadID        string       `json:"thread_id"`
	Retryable       bool         `json:"retryable"`
	Error           *AccessError `json:"error"`
}
type AccessReceipt struct {
	ProtocolVersion int                 `json:"protocol_version"`
	ID              string              `json:"id"`
	TaskID          string              `json:"task_id"`
	RequestID       string              `json:"request_id"`
	Revision        string              `json:"revision"`
	ExternalID      string              `json:"external_id"`
	Decision        AccessDecisionState `json:"decision"`
	Grant           AccessGrant         `json:"grant"`
	Notification    AccessNotification  `json:"notification"`
}
type AccessDecisionSemantics struct {
	Approve string `json:"approve"`
	Reject  string `json:"reject"`
}
type AccessView struct {
	Review             *AccessReview           `json:"review"`
	Outcome            *AccessOutcome          `json:"outcome"`
	ID                 string                  `json:"id"`
	TaskID             string                  `json:"task_id"`
	Kind               string                  `json:"kind"`
	Target             AccessTarget            `json:"target"`
	Revision           string                  `json:"revision"`
	Status             string                  `json:"status"`
	Scope              string                  `json:"scope"`
	GrantExpiresAt     *string                 `json:"grant_expires_at"`
	Snapshot           *AccessSnapshot         `json:"snapshot"`
	SupportedDecisions []string                `json:"supported_decisions"`
	DecisionSemantics  AccessDecisionSemantics `json:"decision_semantics"`
	Actionable         bool                    `json:"actionable"`
	Receipt            *AccessReceipt          `json:"receipt"`
	Error              *AccessError            `json:"error"`
}
type AccessListResponse struct {
	ProtocolVersion int           `json:"protocol_version"`
	Features        []string      `json:"features"`
	TaskID          string        `json:"task_id"`
	Refresh         AccessRefresh `json:"refresh"`
	Advisor         AccessAdvisor `json:"advisor"`
	Requests        []AccessView  `json:"requests"`
	Error           *AccessError  `json:"error"`
}
type AccessShowResponse struct {
	ProtocolVersion int           `json:"protocol_version"`
	Features        []string      `json:"features"`
	TaskID          string        `json:"task_id"`
	Refresh         AccessRefresh `json:"refresh"`
	Advisor         AccessAdvisor `json:"advisor"`
	Request         *AccessView   `json:"request"`
	Error           *AccessError  `json:"error"`
}
type AccessDecisionResponse struct {
	Outcome         *AccessOutcome `json:"outcome"`
	ProtocolVersion int            `json:"protocol_version"`
	TaskID          string         `json:"task_id"`
	RequestID       string         `json:"request_id"`
	Receipt         *AccessReceipt `json:"receipt"`
	Error           *AccessError   `json:"error"`
}

func accessDigest(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	// Canonicalize RawMessage objects as well as Go struct/map fields. UseNumber
	// avoids float64 rounding permission-relevant integer values in opaque rules.
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err = d.Decode(&v); err != nil {
		return "", err
	}
	b, err = json.Marshal(v)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b)), nil
}
func validAccessRevision(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	for _, c := range value[7:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
func (in AccessDecisionInput) validate() *AccessError {
	if in.ProtocolVersion != 1 {
		return accessError("protocol_unsupported", "access protocol_version must be 1", false)
	}
	if !validID(in.TaskID) || !validAccessRevision(in.Revision) || (in.Decision != "approve" && in.Decision != "reject") ||
		len(in.ExternalID) == 0 || len(in.ExternalID) > 256 || strings.ContainsRune(in.ExternalID, '\x00') ||
		strings.TrimSpace(in.Actor.Source) == "" || len(in.Actor.Source) > 128 || strings.TrimSpace(in.Actor.UserID) == "" || len(in.Actor.UserID) > 256 || len(in.Actor.WorkspaceID) > 256 || len(in.Reason) > 4096 || strings.ContainsRune(in.Reason, '\x00') || (in.Decision == "reject" && strings.TrimSpace(in.Reason) == "") {
		return accessError("invalid_input", "invalid task, revision, decision, idempotency key, actor, or reason", false)
	}
	return nil
}

// DecodeAccessDecision rejects duplicate keys and unknown authority fields.
func DecodeAccessDecision(b []byte) (AccessDecisionInput, error) {
	var in AccessDecisionInput
	if err := strictAccessJSON(b, &in); err != nil {
		return in, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	for _, key := range []string{"protocol_version", "task_id", "revision", "decision", "external_id", "actor", "reason"} {
		if fields[key] == nil {
			return in, accessError("invalid_input", "Missing required decision field: "+key, false)
		}
	}
	var actorFields map[string]json.RawMessage
	_ = json.Unmarshal(fields["actor"], &actorFields)
	for _, key := range []string{"source", "workspace_id", "user_id"} {
		if actorFields[key] == nil {
			return in, accessError("invalid_input", "Missing required actor field: "+key, false)
		}
	}
	if err := in.validate(); err != nil {
		return in, err
	}
	return in, nil
}
func strictAccessJSON(b []byte, value any) error {
	if len(b) > maxAccessInput || !utf8.Valid(b) {
		return accessError("invalid_input", "access input must be at most 65536 UTF-8 bytes", false)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if v, ok := t.(json.Delim); ok {
			switch v {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					name, ok := k.(string)
					if !ok || seen[name] {
						return fmt.Errorf("duplicate or invalid JSON key")
					}
					seen[name] = true
					if e = walk(); e != nil {
						return e
					}
				}
			case '[':
				for d.More() {
					if e := walk(); e != nil {
						return e
					}
				}
			default:
				return fmt.Errorf("unexpected JSON delimiter")
			}
			_, err = d.Token()
		}
		return err
	}
	if err := walk(); err != nil {
		return accessError("invalid_input", "invalid or ambiguous access JSON", false)
	}
	if _, err := d.Token(); err != io.EOF {
		return accessError("invalid_input", "access input must contain one JSON object", false)
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return accessError("invalid_input", "unknown or invalid access JSON fields", false)
	}
	return nil
}

// ReadAccessDecisionFile takes a bounded regular-file snapshot. Lstat+SameFile
// rejects symlinks and substitution; nonblocking platform open rejects FIFOs.
func ReadAccessDecisionFile(path string) (AccessDecisionInput, error) {
	var in AccessDecisionInput
	b, err := ReadAccessInputFile(path)
	if err != nil {
		return in, err
	}
	return DecodeAccessDecision(b)
}

func ReadAccessInputFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, accessError("invalid_input", "access input must be a readable regular file", false)
	}
	f, err := openAccessConfigFile(path)
	if err != nil {
		return nil, accessError("invalid_input", "cannot open access input", false)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, accessError("invalid_input", "access input changed while opening", false)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxAccessInput+1))
	if err != nil || len(b) > maxAccessInput {
		return nil, accessError("invalid_input", "cannot read bounded access input", false)
	}
	return b, nil
}
