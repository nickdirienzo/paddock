package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// These review fixtures deliberately exercise the shared access executor rather
// than the adapter RPC layer. They never connect to a live gateway or agent.
type accessReviewBackend struct {
	inspection        OpenShellAccessInspection
	inspectErr        error
	approveErr        error
	approveCalls      int
	approvedToken     string
	approvedRequestID string
	approveHook       func()
}

func (b *accessReviewBackend) Inspect(context.Context, Task) (OpenShellAccessInspection, error) {
	return b.inspection, b.inspectErr
}

func (b *accessReviewBackend) Approve(_ context.Context, _ Task, _ string, token, requestID string) (OpenShellAccessApplication, error) {
	b.approveCalls++
	b.approvedToken, b.approvedRequestID = token, requestID
	if b.approveHook != nil {
		b.approveHook()
	}
	if b.approveErr != nil {
		return OpenShellAccessApplication{}, b.approveErr
	}
	b.inspection.Chunks[0].Status = "approved"
	return OpenShellAccessApplication{Status: "unknown", PolicyVersion: 7, PolicyHash: "policy-hash"}, nil
}

func (*accessReviewBackend) Reject(context.Context, Task, string, string, string) error {
	return errors.New("review fixture never permits reject")
}

func accessReviewFixture(t *testing.T) (Service, Task, *accessReviewBackend, string) {
	t.Helper()
	task := taskFixture()
	task.Runtime, task.Status = "openshell", "running"
	task.Session.Agent = "unsupported"
	task.Session.ConversationID = "original-conversation"
	task.OpenShell = &OpenShellTask{ID: "original-sandbox-id", Name: "pd-review", Ready: true,
		Config: OpenShellConfig{Gateway: "review-gateway", Workspace: "default"}}
	requestID := "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89"
	b := &accessReviewBackend{inspection: OpenShellAccessInspection{SandboxID: task.OpenShell.ID, AdvisorState: "available", ApprovalMode: "manual", ExactRuleBound: true,
		Application: OpenShellAccessApplication{Status: "unknown"}, Chunks: []OpenShellAccessChunk{{
			ID: requestID, Status: "pending", RuleName: "restricted", ReviewToken: "captured-review-token",
			Rule:      json.RawMessage(`{"name":"restricted","endpoints":[{"host":"registry.example","ports":[443],"rules":[{"allow":{"method":"GET","path":"/packages/**"}}]}],"binaries":[{"path":"/usr/bin/node"}]}`),
			Rationale: "Read packages", ValidationResult: "no new findings", CurrentEffectivePolicyHash: "current-hash", CandidateEffectivePolicyHash: "candidate-hash",
		}}}}
	store := openTestStore(t, t.TempDir())
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	return Service{Store: store, accessBackend: b}, task, b, requestID
}

func accessReviewInput(t *testing.T, s Service, task Task, requestID string) AccessDecisionInput {
	t.Helper()
	view := s.ShowAccessV1(context.Background(), task.ID, requestID, true)
	if view.Error != nil || view.Request == nil || !view.Request.Actionable {
		t.Fatalf("request not actionable: %+v", view)
	}
	return AccessDecisionInput{ProtocolVersion: 1, TaskID: task.ID, Revision: view.Request.Revision, Decision: "approve", ExternalID: "review-action", Actor: AccessActor{Source: "arnold-slack", WorkspaceID: "T1", UserID: "U1"}, Reason: "Required packages"}
}

func TestAccessReviewChangedCompleteRuleCannotUseOldRevision(t *testing.T) {
	s, task, b, requestID := accessReviewFixture(t)
	in := accessReviewInput(t, s, task, requestID)
	// Change only a permission-relevant L7 path; keep the opaque token unchanged
	// to ensure Paddock's full-rule digest is independently enforced.
	b.inspection.Chunks[0].Rule = json.RawMessage(strings.ReplaceAll(string(b.inspection.Chunks[0].Rule), "/packages/**", "/**"))
	response := s.DecideAccessV1(context.Background(), requestID, in)
	if response.Error == nil || response.Error.Code != "proposal_changed" || b.approveCalls != 0 {
		t.Fatalf("changed rule was not rejected without execution: %+v calls=%d", response, b.approveCalls)
	}
	if response.Receipt == nil || response.Receipt.Decision.State != "stale" {
		t.Fatalf("missing durable stale receipt: %+v", response)
	}
	replay := s.DecideAccessV1(context.Background(), requestID, in)
	if replay.Receipt == nil || replay.Receipt.ID != response.Receipt.ID || b.approveCalls != 0 {
		t.Fatalf("stale input replay changed identity/executed: %+v", replay)
	}
}

func TestAccessReviewCapturedTokenPassedVerbatimAndBackendStaleNotRetried(t *testing.T) {
	s, task, b, requestID := accessReviewFixture(t)
	in := accessReviewInput(t, s, task, requestID)
	b.approveErr = &OpenShellAccessBackendError{Code: "proposal_changed", OutcomeUncertain: false}
	response := s.DecideAccessV1(context.Background(), requestID, in)
	if response.Error == nil || response.Receipt == nil || response.Receipt.Decision.State != "stale" {
		t.Fatalf("backend stale response not preserved: %+v", response)
	}
	if b.approvedToken != "captured-review-token" || !validAccessID(b.approvedRequestID) {
		t.Fatalf("wrong stored token/request UUID: %q %q", b.approvedToken, b.approvedRequestID)
	}
	if b.approveCalls != 1 {
		t.Fatalf("unexpected backend calls: %d", b.approveCalls)
	}
	replay := s.DecideAccessV1(context.Background(), requestID, in)
	if replay.Receipt == nil || replay.Receipt.ID != response.Receipt.ID || b.approveCalls != 1 {
		t.Fatalf("stale backend action replayed: %+v calls=%d", replay, b.approveCalls)
	}
}

func TestAccessReviewUncertainDecisionReconcileNeverReissuesOrNotifies(t *testing.T) {
	s, task, b, requestID := accessReviewFixture(t)
	in := accessReviewInput(t, s, task, requestID)
	notifications := 0
	s.messageDelivery = func(context.Context, Task, string) (string, error) { notifications++; return "thread", nil }
	b.approveErr = &OpenShellAccessBackendError{Code: "outcome_uncertain", OutcomeUncertain: true}
	response := s.DecideAccessV1(context.Background(), requestID, in)
	if response.Error == nil || response.Receipt == nil || response.Receipt.Decision.State != "uncertain" {
		t.Fatalf("expected uncertainty: %+v", response)
	}
	// Upstream now shows approved, but that alone does not prove this admitted
	// request caused it. Do not invent attribution or send a second continuation.
	b.inspection.Chunks[0].Status = "approved"
	for i := 0; i < 2; i++ {
		replay := s.DecideAccessV1(context.Background(), requestID, in)
		if replay.Receipt == nil || replay.Receipt.ID != response.Receipt.ID {
			t.Fatalf("lost receipt: %+v", replay)
		}
		reconciled := s.ReconcileAccessV1(context.Background(), task.ID, requestID)
		if reconciled.Receipt == nil || reconciled.Receipt.Decision.State != "uncertain" {
			t.Fatalf("invented confirmed outcome: %+v", reconciled)
		}
	}
	if b.approveCalls != 1 || notifications != 0 {
		t.Fatalf("uncertain operation was replayed or notified: calls=%d notifications=%d", b.approveCalls, notifications)
	}
}

func TestAccessReviewMissingProposalRetainsUncertaintyAndWorkspace(t *testing.T) {
	s, task, b, requestID := accessReviewFixture(t)
	accessReviewInput(t, s, task, requestID)
	b.inspection.Chunks = []OpenShellAccessChunk{}
	view := s.ShowAccessV1(context.Background(), task.ID, requestID, true)
	if view.Request == nil || view.Request.Actionable || view.Request.Status == "withdrawn" || view.Request.Status == "superseded" {
		t.Fatalf("absence invented terminal outcome: %+v", view)
	}
	if view.Request.Error == nil {
		t.Fatalf("missing proposal has no explicit uncertainty: %+v", view.Request)
	}
	if err := s.noPendingAccessDecisions(task.ID); err == nil {
		t.Fatal("unknown proposal no longer protects workspace retention")
	}
}

func TestAccessReviewReplayDoesNotHideMessageReceiptReadFailure(t *testing.T) {
	s, task, _, requestID := accessReviewFixture(t)
	if err := s.Store.update(task.ID, func(v *Task) error {
		v.Session.Agent = "codex"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.messageDelivery = func(context.Context, Task, string) (string, error) {
		return "", ErrAgentOffline
	}
	in := accessReviewInput(t, s, task, requestID)
	response := s.DecideAccessV1(context.Background(), requestID, in)
	if response.Error != nil || response.Receipt == nil {
		t.Fatalf("decision failed: %+v", response)
	}
	n := response.Receipt.Notification
	if n.ExternalID == "" || n.Status != "pending" {
		t.Fatalf("expected pending deterministic notification receipt: %+v", n)
	}
	path := s.Store.messageReceiptPath(task.ID, accessNotificationCaller, n.ExternalID)
	if err := os.WriteFile(path, []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	replay := s.DecideAccessV1(context.Background(), requestID, in)
	if replay.Error == nil || replay.Error.Code != "storage_error" {
		t.Fatalf("receipt corruption was hidden: %+v", replay)
	}
}

func TestAccessReviewCorruptedCurrentSnapshotFailsClosed(t *testing.T) {
	s, task, _, requestID := accessReviewFixture(t)
	accessReviewInput(t, s, task, requestID)
	if err := s.Store.updateAccessState(task.ID, func(v *accessTaskState) error {
		v.Requests[requestID].View.Snapshot.Rationale = "corrupted displayed snapshot"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.accessState(task.ID); err == nil {
		t.Fatal("current view no longer matches immutable revision but ledger was accepted")
	}
}

func TestAccessReviewExpectedMessageTargetIsCheckedUnderOperationLease(t *testing.T) {
	changes := map[string]func(*Task){
		"sandbox":      func(v *Task) { v.OpenShell.ID = "replacement-sandbox" },
		"conversation": func(v *Task) { v.Session.ConversationID = "replacement-conversation" },
		"workspace":    func(v *Task) { v.OpenShell.Config.Workspace = "other-workspace" },
		"removed":      func(v *Task) { v.WorkspaceRemovedAt = now() },
		"reviving":     func(v *Task) { v.Archive = &TaskArchiveState{Phase: "reviving"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, expected, _, _ := accessReviewFixture(t)
			expected.Session.Agent = "codex"
			if err := s.Store.update(expected.ID, func(v *Task) error {
				v.Session.Agent = "codex"
				change(v)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// No terminal/backend is configured: the expected-target check must
			// reject before Refresh, resume, or native queue access can be attempted.
			thread, err := s.messageExpected(context.Background(), expected, "decision notification", &expected)
			if !errors.Is(err, ErrMessageTargetChanged) || thread != "" {
				t.Fatalf("replacement target reached delivery: thread=%q err=%v", thread, err)
			}
		})
	}
}

func TestAccessReviewMessageReceiptOverridesStaleOutboxDeliveryBit(t *testing.T) {
	s, task, _, requestID := accessReviewFixture(t)
	if err := s.Store.update(task.ID, func(v *Task) error { v.Session.Agent = "codex"; return nil }); err != nil {
		t.Fatal(err)
	}
	sends := 0
	s.messageDelivery = func(context.Context, Task, string) (string, error) { sends++; return "native-thread", nil }
	in := accessReviewInput(t, s, task, requestID)
	response := s.DecideAccessV1(context.Background(), requestID, in)
	if response.Error != nil || response.Receipt == nil || response.Receipt.Notification.Status != "delivered" {
		t.Fatalf("decision notification failed: %+v", response)
	}
	if err := s.Store.updateAccessState(task.ID, func(v *accessTaskState) error {
		v.Decisions[response.Receipt.ID].Receipt.Notification.Status = "pending"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAccessOutboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("stale aggregate bit duplicated native delivery: %d sends", sends)
	}
	replay := s.DecideAccessV1(context.Background(), requestID, in)
	if replay.Error != nil || replay.Receipt == nil || replay.Receipt.Notification.Status != "delivered" {
		t.Fatalf("authoritative message receipt was not surfaced: %+v", replay)
	}
}

func TestAccessReviewClaimedNativeReceiptIsNeverAutomaticallyReplayed(t *testing.T) {
	s, task, _, requestID := accessReviewFixture(t)
	if err := s.Store.update(task.ID, func(v *Task) error { v.Session.Agent = "codex"; return nil }); err != nil {
		t.Fatal(err)
	}
	sends := 0
	s.messageDelivery = func(context.Context, Task, string) (string, error) { sends++; return "", ErrAgentOffline }
	in := accessReviewInput(t, s, task, requestID)
	response := s.DecideAccessV1(context.Background(), requestID, in)
	if response.Error != nil || response.Receipt == nil || response.Receipt.Notification.Status != "pending" {
		t.Fatalf("expected definitely-unsent pending receipt: %+v", response)
	}
	state, err := s.Store.accessState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := state.Decisions[response.Receipt.ID]
	_, claimed, err := s.Store.ClaimMessage(task.ID, accessNotificationCaller, d.Receipt.Notification.ExternalID, d.NotificationBody)
	if err != nil || !claimed {
		t.Fatalf("could not simulate interrupted native claim: claimed=%v err=%v", claimed, err)
	}
	if err := s.ReconcileAccessOutboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("ambiguous claimed receipt was automatically replayed: %d sends", sends)
	}
	replay := s.DecideAccessV1(context.Background(), requestID, in)
	if replay.Receipt == nil || replay.Receipt.Notification.Status != "claimed" || replay.Receipt.Notification.Retryable {
		t.Fatalf("claimed uncertainty not preserved: %+v", replay)
	}
}

func TestAccessReviewTwoStoreConnectionsHaveOneDurableWinner(t *testing.T) {
	s, task, b, requestID := accessReviewFixture(t)
	in := accessReviewInput(t, s, task, requestID)
	other := Service{Store: openTestStore(t, s.Store.Home), accessBackend: b}
	entered, release := make(chan struct{}), make(chan struct{})
	b.approveHook = func() { close(entered); <-release }
	completed := make(chan AccessDecisionResponse, 1)
	go func() { completed <- s.DecideAccessV1(context.Background(), requestID, in) }()
	<-entered
	competing := in
	competing.ExternalID = "other-review-action"
	busy := other.DecideAccessV1(context.Background(), requestID, competing)
	if busy.Error == nil || busy.Error.Code != "task_busy" || busy.Receipt != nil {
		close(release)
		<-completed
		t.Fatalf("concurrent operation was admitted through another store: %+v", busy)
	}
	close(release)
	winner := <-completed
	if winner.Error != nil || winner.Receipt == nil || winner.Receipt.Decision.State != "confirmed" {
		t.Fatalf("winning decision failed: %+v", winner)
	}
	loser := other.DecideAccessV1(context.Background(), requestID, competing)
	if loser.Error == nil || loser.Error.Code != "decision_conflict" || loser.Receipt == nil {
		t.Fatalf("later competing event displaced winner: %+v", loser)
	}
	if b.approveCalls != 1 {
		t.Fatalf("multiple backend executions across store connections: %d", b.approveCalls)
	}
	state, err := other.Store.accessState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Requests[requestID].Winner != winner.Receipt.ID {
		t.Fatalf("durable winner changed: %+v", state.Requests[requestID])
	}
}

func TestAccessReviewUnqualifiedBackendCannotBeEnabledByCompleteSnapshot(t *testing.T) {
	s, task, b, requestID := accessReviewFixture(t)
	b.inspection.ExactRuleBound = false
	show := s.ShowAccessV1(context.Background(), task.ID, requestID, true)
	if show.Error != nil || show.Request == nil || show.Request.Snapshot == nil || show.Request.Actionable || len(show.Request.SupportedDecisions) != 0 {
		t.Fatalf("unqualified backend advertised actions: %+v", show)
	}
	in := AccessDecisionInput{ProtocolVersion: 1, TaskID: task.ID, Revision: show.Request.Revision, Decision: "approve", ExternalID: "unqualified-action",
		Actor: AccessActor{Source: "arnold-slack", WorkspaceID: "T1", UserID: "U1"}, Reason: "Snapshot is complete but native guard is not qualified"}
	response := s.DecideAccessV1(context.Background(), requestID, in)
	if response.Error == nil || response.Error.Code != "precondition_unavailable" || b.approveCalls != 0 {
		t.Fatalf("unqualified backend accepted a decision: %+v calls=%d", response, b.approveCalls)
	}
}
