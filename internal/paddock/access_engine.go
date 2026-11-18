package paddock

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

func (s Service) accessTransport() OpenShellAccessBackend {
	if s.accessBackend != nil {
		return s.accessBackend
	}
	return NewOpenShellAccessBackend()
}
func newAccessUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func accessBackendError(err error) *AccessError {
	var e *OpenShellAccessBackendError
	if errors.As(err, &e) {
		return accessError(e.Code, e.Error(), e.Retryable)
	}
	return accessError("backend_unavailable", "Authoritative access backend is unavailable; inspect the host connection.", true)
}
func (s Service) authorizedAccessTask(taskID string) (Task, *AccessError) {
	if !validID(taskID) {
		return Task{}, accessError("invalid_input", "A full task ID is required.", false)
	}
	t, err := s.Store.Get(taskID)
	if err != nil {
		return t, accessError("task_unavailable", "Task is unavailable.", false)
	}
	caller, err := LocalCaller()
	if err != nil {
		return t, accessError("unauthorized", "Cannot authenticate local OS caller.", false)
	}
	if t.Provenance.Caller != "" && t.Provenance.Caller != caller {
		return t, accessError("unauthorized", "Local OS caller does not own this task.", false)
	}
	return t, nil
}
func usableAccessTask(t Task) *AccessError {
	if t.RuntimeName() != "openshell" || t.OpenShell == nil || !t.OpenShell.Ready || t.OpenShell.ID == "" {
		return accessError("precondition_unavailable", "Task has no verified retained OpenShell sandbox identity.", false)
	}
	if t.WorkspaceRemovedAt != "" || (t.Archive != nil && t.Archive.Phase != "" && t.Archive.Phase != "revived") {
		return accessError("target_replaced", "Task workspace has been removed or its lifecycle is changing.", false)
	}
	return nil
}

// refreshAccessLocked performs observations only. The caller holds the task
// operation lease. It never grants, wakes an agent, or dispatches an outbox.
func (s Service) refreshAccessLocked(ctx context.Context, t Task) (OpenShellAccessInspection, *AccessError) {
	var observation OpenShellAccessInspection
	if e := usableAccessTask(t); e != nil {
		return observation, e
	}
	var err error
	observation, err = s.accessTransport().Inspect(ctx, t)
	if err != nil {
		failure := accessBackendError(err)
		// Persist authoritative target loss to fence later native notifications.
		if failure.Code == "target_replaced" || failure.Code == "target_unavailable" {
			if err := s.blockAccessTarget(t, failure); err != nil {
				return observation, accessError("storage_error", "Could not persist unavailable target identity.", true)
			}
			return observation, failure
		}
		return observation, failure
	}
	if observation.SandboxID != t.OpenShell.ID {
		failure := accessError("target_replaced", "OpenShell sandbox ID no longer matches the task.", false)
		if err := s.blockAccessTarget(t, failure); err != nil {
			return observation, accessError("storage_error", "Could not persist changed target identity.", true)
		}
		return observation, failure
	}
	err = s.Store.updateAccessState(t.ID, func(state *accessTaskState) error {
		// A successful authoritative observation confirms this sandbox identity.
		state.TargetBlock = nil
		state.CheckedAt = now()
		state.Advisor = AccessAdvisor{State: observation.AdvisorState}
		if state.Advisor.State != "available" && state.Advisor.State != "unavailable" {
			state.Advisor.State = "unknown"
		}
		if observation.AdvisorError != "" {
			state.Advisor.Error = accessError("advisor_unavailable", "OpenShell policy advisor availability could not be confirmed.", true)
		}
		seen := map[string]bool{}
		for _, chunk := range observation.Chunks {
			if !validAccessID(chunk.ID) {
				return fmt.Errorf("invalid upstream access chunk identity")
			}
			seen[chunk.ID] = true
			if chunk.Status != "pending" && chunk.Status != "approved" && chunk.Status != "rejected" {
				return fmt.Errorf("unknown upstream access state")
			}
			stored := state.Requests[chunk.ID]
			if stored == nil {
				stored = &accessStoredRequest{View: AccessView{ID: chunk.ID, TaskID: t.ID, Kind: "network", Scope: "sandbox"}, Snapshots: map[string]accessStoredSnapshot{}}
				state.Requests[chunk.ID] = stored
			}
			if stored.View.Kind != "network" {
				return fmt.Errorf("unsupported access request kind")
			}
			stored.View.Target = accessTargetFor(t)
			stored.View.Status = chunk.Status
			stored.View.SupportedDecisions = []string{}
			stored.View.DecisionSemantics = AccessDecisionSemantics{Approve: "unavailable", Reject: "unavailable"}
			stored.View.Error = nil
			if len(chunk.Rule) == 0 || string(chunk.Rule) == "null" || chunk.ReviewToken == "" {
				// Terminal chunks can lose their token; preserve the last immutable
				// snapshot for audit, never synthesize an actionable one.
				if chunk.Status == "pending" {
					stored.View.Error = accessError("precondition_unavailable", "Complete rule and review token are required.", false)
				}
				continue
			}
			var rule map[string]any
			decoder := json.NewDecoder(strings.NewReader(string(chunk.Rule)))
			decoder.UseNumber()
			if err := decoder.Decode(&rule); err != nil || rule == nil {
				return fmt.Errorf("invalid structured network rule")
			}
			tokenDigest, _ := accessDigest(chunk.ReviewToken)
			security := []string{}
			if chunk.SecurityNotes != "" {
				security = append(security, chunk.SecurityNotes)
			}
			snapshot := AccessSnapshot{SchemaVersion: 1, TaskID: t.ID, RequestID: chunk.ID, Kind: "network", Target: accessTargetFor(t), Rule: chunk.Rule, RuleName: chunk.RuleName, Rationale: chunk.Rationale, Security: security, ValidationResult: chunk.ValidationResult, ApplicationError: chunk.ApplicationError, CurrentEffectivePolicyHash: chunk.CurrentEffectivePolicyHash, CandidateEffectivePolicyHash: chunk.CandidateEffectivePolicyHash, ReviewTokenDigest: tokenDigest}
			revision, err := accessDigest(snapshot)
			if err != nil {
				return err
			}
			if old, exists := stored.Snapshots[revision]; exists {
				if old.ReviewToken != chunk.ReviewToken {
					return fmt.Errorf("immutable access snapshot changed")
				}
			} else {
				stored.Snapshots[revision] = accessStoredSnapshot{snapshot, chunk.ReviewToken}
			}
			stored.View.Snapshot = &snapshot
			stored.View.Revision = revision
			if !observation.ExactRuleBound {
				stored.View.Error = accessError("precondition_unavailable", "This backend cannot bind approval to the exact reviewed rule and sandbox.", false)
			} else if chunk.QualificationError != "" {
				stored.View.Error = accessError("rule_scope_unsupported", chunk.QualificationError, false)
			} else if chunk.ApplicationError != "" {
				stored.View.Error = accessError("application_error", "OpenShell reports a proposal application error; inspect the snapshot.", false)
			} else {
				stored.View.DecisionSemantics.Approve = "exact_reviewed_rule"
				stored.View.SupportedDecisions = append(stored.View.SupportedDecisions, "approve")
			}
			if observation.RejectPendingOnly {
				stored.View.DecisionSemantics.Reject = "current_pending_request"
				stored.View.SupportedDecisions = append(stored.View.SupportedDecisions, "reject")
			}
		}
		for _, chunk := range observation.Chunks {
			if chunk.SupersedesChunkID != "" {
				if old := state.Requests[chunk.SupersedesChunkID]; old != nil && old.View.Status == "pending" {
					old.View.Status = "superseded"
					old.View.SupportedDecisions = []string{}
				}
			}
		}
		for id, r := range state.Requests {
			if r.View.Kind == "network" && !seen[id] && (r.View.Status == "pending" || r.View.Status == "unknown") {
				r.View.Status = "unknown"
				r.View.SupportedDecisions = []string{}
				r.View.Error = accessError("proposal_missing", "Proposal is absent; no terminal outcome has been proven.", false)
			}
		}
		return nil
	})
	if err != nil {
		return observation, accessError("storage_error", "Could not persist authoritative access observation.", true)
	}
	return observation, nil
}

func (s Service) ListAccessV1(ctx context.Context, taskID string, refresh bool) AccessListResponse {
	out := AccessListResponse{ProtocolVersion: 1, Features: append([]string{}, accessFeatures...), TaskID: taskID, Refresh: AccessRefresh{State: "cached"}, Advisor: AccessAdvisor{State: "unknown"}, Requests: []AccessView{}}
	t, e := s.authorizedAccessTask(taskID)
	if e != nil {
		out.Error = e
		out.Refresh = AccessRefresh{State: "error", Error: e}
		return out
	}
	if refresh {
		release, err := s.Store.taskLease(t.ID, "operation")
		if err != nil {
			out.Error = accessError("task_busy", "Task has an active operation; retry inspection.", true)
		} else {
			t, out.Error = s.authorizedAccessTask(taskID)
			if out.Error == nil {
				_, out.Error = s.refreshAccessLocked(ctx, t)
			}
			release()
		}
		if out.Error != nil {
			out.Refresh = AccessRefresh{State: "error", Error: out.Error}
		} else {
			out.Refresh.State = "fresh"
		}
	}
	state, err := s.Store.accessState(taskID)
	if err != nil {
		out.Error = accessError("storage_error", "Could not read access ledger.", true)
		out.Refresh = AccessRefresh{State: "error", Error: out.Error}
		return out
	}
	if err = s.Store.refreshAccessReceiptViews(&state); err != nil {
		out.Error = accessError("storage_error", "Could not reconcile message receipt identity.", false)
		out.Refresh = AccessRefresh{State: "error", Error: out.Error}
	}
	out.Advisor = state.Advisor
	if out.Refresh.CheckedAt == "" {
		out.Refresh.CheckedAt = state.CheckedAt
	}
	out.Requests = accessStateViews(state, out.Refresh.State == "fresh")
	for i := range out.Requests {
		out.Requests[i] = presentAccessView(out.Requests[i])
	}
	return out
}
func (s Service) ShowAccessV1(ctx context.Context, taskID, requestID string, refresh bool) AccessShowResponse {
	list := s.ListAccessV1(ctx, taskID, refresh)
	out := AccessShowResponse{ProtocolVersion: 1, Features: list.Features, TaskID: taskID, Refresh: list.Refresh, Advisor: list.Advisor, Error: list.Error}
	if !validAccessID(requestID) {
		out.Error = accessError("invalid_input", "A full access request ID is required.", false)
		return out
	}
	for i := range list.Requests {
		if list.Requests[i].ID == requestID {
			out.Request = &list.Requests[i]
			return out
		}
	}
	if out.Error == nil {
		out.Error = accessError("request_unavailable", "Request does not belong to the supplied task.", false)
	}
	return out
}

func initialAccessReceipt(id, requestID, caller string, in AccessDecisionInput) AccessReceipt {
	return AccessReceipt{ProtocolVersion: 1, ID: id, TaskID: in.TaskID, RequestID: requestID, Revision: in.Revision, ExternalID: in.ExternalID, Decision: AccessDecisionState{Value: in.Decision, Caller: caller, Actor: in.Actor, Reason: in.Reason, State: "claimed", CreatedAt: now(), UpdatedAt: now()}, Grant: AccessGrant{Status: "pending", ApplicationStatus: "unknown"}, Notification: AccessNotification{Status: "not_requested"}}
}
func accessAllows(v AccessView, decision string) bool {
	for _, d := range v.SupportedDecisions {
		if d == decision {
			return true
		}
	}
	return false
}

// DecideAccessV1 is the sole admission/execution path, including legacy CLI.
func (s Service) DecideAccessV1(ctx context.Context, requestID string, in AccessDecisionInput) (out AccessDecisionResponse) {
	defer func() { out = out.WithOutcome() }()
	out = AccessDecisionResponse{ProtocolVersion: 1, TaskID: in.TaskID, RequestID: requestID}
	if e := in.validate(); e != nil {
		out.Error = e
		return out
	}
	if !validAccessID(requestID) {
		out.Error = accessError("invalid_input", "A full access request ID is required.", false)
		return out
	}
	t, e := s.authorizedAccessTask(in.TaskID)
	if e != nil {
		out.Error = e
		return out
	}
	caller, err := LocalCaller()
	if err != nil {
		out.Error = accessError("unauthorized", "Cannot authenticate OS caller.", false)
		return out
	}
	release, err := s.Store.taskLease(t.ID, "operation")
	if err != nil {
		out.Error = accessError("task_busy", "Task has an active operation; retry this same external ID.", true)
		return out
	}
	// Explicit release before outbox delivery; Message and ClaimMessage each
	// acquire this lease. Idempotency is checked BEFORE current proposal status.
	done := false
	defer func() {
		if !done {
			release()
		}
	}()
	t, e = s.authorizedAccessTask(in.TaskID)
	if e != nil {
		out.Error = e
		return out
	}
	state, err := s.Store.accessState(t.ID)
	if err != nil {
		out.Error = accessError("storage_error", "Could not read access ledger.", true)
		return out
	}
	key := accessKey(caller, in.ExternalID)
	if id := state.Idempotency[key]; id != "" {
		d := state.Decisions[id]
		if d == nil || d.Receipt.RequestID != requestID || !reflect.DeepEqual(d.Input, in) {
			out.Error = accessError("idempotency_conflict", "External ID already belongs to different decision content.", false)
			return out
		}
		receipt := d.Receipt
		n, err := s.Store.accessNotificationView(*d)
		if err != nil {
			out.Error = accessError("storage_error", "Cannot read notification receipt.", true)
		} else {
			receipt.Notification = n
			out.Error = receipt.Decision.Error
		}
		out.Receipt = &receipt
		return out
	}
	if e = usableAccessTask(t); e != nil {
		out.Error = e
		return out
	}
	stored := state.Requests[requestID]
	if stored == nil {
		out.Error = accessError("request_unavailable", "Inspect this task's request before deciding it.", false)
		return out
	}
	var observation OpenShellAccessInspection
	if stored.View.Kind == "network" {
		observation, e = s.refreshAccessLocked(ctx, t)
		if e != nil {
			out.Error = e
			return out
		}
	}
	state, err = s.Store.accessState(t.ID)
	if err != nil {
		out.Error = accessError("storage_error", "Cannot reread authoritative access ledger.", true)
		return out
	}
	stored = state.Requests[requestID]
	if stored == nil {
		out.Error = accessError("request_unavailable", "Request is unavailable.", false)
		return out
	}
	id := newID()
	d := &accessStoredDecision{Receipt: initialAccessReceipt(id, requestID, caller, in), Input: in, BackendRequestID: newAccessUUID(), ConversationID: t.Session.ConversationID}
	if stored.Winner != "" {
		d.Receipt.Decision.State = "conflict"
		d.Receipt.Decision.Error = accessError("decision_conflict", "Another decision already owns this request.", false)
	} else if stored.View.Revision != in.Revision || stored.View.Status != "pending" || stored.View.Target != accessTargetFor(t) {
		d.Receipt.Decision.State = "stale"
		d.Receipt.Decision.Error = accessError("proposal_changed", "Reviewed revision or target is no longer pending; review it again.", false)
	} else if !accessAllows(stored.View, in.Decision) {
		d.Receipt.Decision.State = "stale"
		d.Receipt.Decision.Error = accessError("precondition_unavailable", "This backend cannot safely perform the requested decision.", false)
	}
	if d.Receipt.Decision.Error == nil {
		d.Receipt.Decision.State = "applying"
	}

	err = s.Store.updateAccessState(t.ID, func(latest *accessTaskState) error {
		if latest.Idempotency[key] != "" {
			return fmt.Errorf("decision idempotency claim changed")
		}
		request := latest.Requests[requestID]
		if request == nil || request.Winner != stored.Winner {
			return fmt.Errorf("decision winner changed")
		}
		latest.Decisions[id] = d
		latest.Idempotency[key] = id
		if d.Receipt.Decision.Error == nil {
			request.Winner = id
			if d.BackendAcknowledged {
				request.View.Status = d.Receipt.Grant.Status
			}
		}
		return nil
	})
	if err != nil {
		out.Error = accessError("storage_error", "Could not durably claim access decision; no backend action attempted.", true)
		return out
	}
	if d.Receipt.Decision.Error != nil {
		r := d.Receipt
		out.Receipt = &r
		out.Error = r.Decision.Error
		return out
	}
	{
		snapshot := stored.Snapshots[in.Revision]
		var callErr error
		if in.Decision == "approve" {
			app, e := s.accessTransport().Approve(ctx, t, requestID, snapshot.ReviewToken, d.BackendRequestID)
			callErr = e
			if e == nil {
				d.Receipt.Grant = AccessGrant{Status: "approved", ApplicationStatus: app.Status, PolicyVersion: app.PolicyVersion, PolicyHash: app.PolicyHash}
				if app.Error != "" {
					d.Receipt.Grant.Error = accessError("application_unconfirmed", app.Error, true)
				}
				if d.Receipt.Grant.ApplicationStatus == "" {
					d.Receipt.Grant.ApplicationStatus = "unknown"
				}
			}
		} else if observation.RejectPendingOnly {
			callErr = s.accessTransport().Reject(ctx, t, requestID, d.BackendRequestID, in.Reason)
			if callErr == nil {
				d.Receipt.Grant = AccessGrant{Status: "rejected", ApplicationStatus: "not_applicable"}
			}
		} else {
			callErr = accessError("precondition_unavailable", "Backend reject is not pending-only.", false)
		}
		if callErr == nil {
			d.BackendAcknowledged = true
			d.Receipt.Decision.State = "confirmed"
			d.Receipt.Decision.Error = nil
		} else {
			be := accessBackendError(callErr)
			var typed *OpenShellAccessBackendError
			if errors.As(callErr, &typed) && !typed.OutcomeUncertain {
				d.Receipt.Decision.State = "stale"
			} else {
				d.Receipt.Decision.State = "uncertain"
				be = accessError("outcome_uncertain", "Access decision may have taken effect; reconcile before any further action.", false)
			}
			d.Receipt.Decision.Error = be
			d.Receipt.Grant.Status = "unknown"
			d.Receipt.Grant.Error = be
		}
	}
	d.Receipt.Decision.UpdatedAt = now()
	if d.BackendAcknowledged {
		s.prepareAccessNotification(t, stored.View, d)
	}
	err = s.Store.updateAccessState(t.ID, func(current *accessTaskState) error {
		current.Decisions[id] = d
		if d.Receipt.Decision.State == "stale" {
			if current.Requests[requestID].Winner == id {
				current.Requests[requestID].Winner = ""
			}
		} else if d.BackendAcknowledged {
			current.Requests[requestID].View.Status = d.Receipt.Grant.Status
		}
		return nil
	})
	if err != nil {
		r := d.Receipt
		out.Receipt = &r
		out.Error = accessError("outcome_uncertain", "Backend may have acted; saving the outcome failed. Reconcile the durable intent.", false)
		return out
	}
	release()
	done = true
	if d.BackendAcknowledged {
		s.deliverAccessNotification(ctx, t, id)
	}
	return s.accessDecisionResponse(t.ID, requestID, id)
}

func (s Service) accessDecisionResponse(taskID, requestID, id string) (out AccessDecisionResponse) {
	defer func() { out = out.WithOutcome() }()
	out = AccessDecisionResponse{ProtocolVersion: 1, TaskID: taskID, RequestID: requestID}
	state, err := s.Store.accessState(taskID)
	if err != nil || state.Decisions[id] == nil {
		out.Error = accessError("storage_error", "Could not read durable decision receipt.", true)
		return out
	}
	d := state.Decisions[id]
	r := d.Receipt
	n, err := s.Store.accessNotificationView(*d)
	if err != nil {
		out.Error = accessError("storage_error", "Could not read notification receipt.", true)
	} else {
		r.Notification = n
	}
	out.Receipt = &r
	if out.Error == nil {
		out.Error = r.Decision.Error
	}
	return out
}
func (s Service) prepareAccessNotification(t Task, v AccessView, d *accessStoredDecision) {
	if d.NotificationBody != "" {
		return
	}
	body := "Paddock access decision " + d.Receipt.ID + " for request " + v.ID + ": "
	if d.Input.Decision == "reject" {
		body += "rejected. Reason: " + d.Input.Reason + ". Adapt to that decision and report any limitation."
	} else {
		body += "OpenShell approved the exact reviewed network rule. Runtime application status: " + d.Receipt.Grant.ApplicationStatus + ". Inspect current state before retrying required work so completed effects are not duplicated. If access is still denied, report the current application state."
	}
	d.NotificationBody = body
	d.Receipt.Notification = AccessNotification{Status: "pending", ExternalID: "access:" + d.Receipt.ID, CallerNamespace: accessNotificationCaller, Retryable: true}
	if t.Session.ConversationID == "" {
		d.Receipt.Notification.Status = "unavailable"
		d.Receipt.Notification.Retryable = false
		d.Receipt.Notification.Error = accessError("no_conversation", "Task has no saved native conversation; notification cannot create one.", false)
	}
}

func (s Service) deliverAccessNotification(ctx context.Context, t Task, id string) {
	state, err := s.Store.accessState(t.ID)
	if err != nil {
		return
	}
	d := state.Decisions[id]
	if d == nil || !d.BackendAcknowledged || d.NotificationBody == "" {
		return
	}
	n, err := s.Store.accessNotificationView(*d)
	if err != nil {
		return
	}
	if n.Status != "pending" {
		return
	}
	current, err := s.Store.Get(t.ID)
	if err != nil {
		return
	}
	if accessTargetFailure(state, current) != nil || current.OpenShell == nil || accessTargetFor(current) != state.Requests[d.Receipt.RequestID].View.Target || current.WorkspaceRemovedAt != "" || current.Session.ConversationID != d.ConversationID || (current.Archive != nil && current.Archive.Phase != "" && current.Archive.Phase != "revived") {
		s.disposeAccessNotification(t.ID, id, "target_replaced")
		return
	}
	if !nativeMessageSupported(current) {
		s.disposeAccessNotification(t.ID, id, "agent_unsupported")
		return
	}
	receipt, claimed, err := s.Store.ClaimMessage(t.ID, accessNotificationCaller, n.ExternalID, d.NotificationBody)
	if err != nil {
		return
	} // outbox remains durable pending even before first claim
	if !claimed {
		return
	}
	var thread string
	var deliveryErr error
	if s.messageDelivery != nil {
		thread, deliveryErr = s.messageDelivery(ctx, current, d.NotificationBody)
	} else {
		thread, deliveryErr = s.messageExpected(ctx, current, d.NotificationBody, &current)
	}
	status, message := "delivered", ""
	if deliveryErr != nil {
		status = "uncertain"
		message = "Native access notification outcome is uncertain."
		if errors.Is(deliveryErr, ErrAgentOffline) {
			status = "pending"
			message = "Agent is definitely offline; retry this same notification key."
		}
	}
	if errors.Is(deliveryErr, ErrMessageTargetChanged) {
		_, finishErr := s.Store.FinishMessage(receipt, "pending", "", "Pinned message target changed before delivery.")
		if finishErr == nil {
			s.disposeAccessNotification(t.ID, id, "target_replaced")
		}
		return
	}
	_, _ = s.Store.FinishMessage(receipt, status, thread, message) // failure retains claimed; never blindly replay
}

// Serialize terminal disposition with ClaimMessage and native messaging. Recheck
// both the reason and the authoritative receipt; a stale pending observation may
// never hide a subsequent claimed, uncertain, or delivered message receipt.
func (s Service) disposeAccessNotification(taskID, id, code string) {
	release, err := s.Store.taskLease(taskID, "operation")
	if err != nil {
		return
	}
	defer release()
	current, err := s.Store.Get(taskID)
	if err != nil {
		return
	}
	_ = s.Store.updateAccessState(taskID, func(state *accessTaskState) error {
		d := state.Decisions[id]
		if d == nil {
			return fmt.Errorf("access notification decision is missing")
		}
		n, err := s.Store.accessNotificationView(*d)
		if err != nil || n.Status != "pending" {
			return err
		}
		if code == "target_replaced" {
			if accessTargetFailure(*state, current) == nil && current.OpenShell != nil && accessTargetFor(current) == state.Requests[d.Receipt.RequestID].View.Target && current.WorkspaceRemovedAt == "" && current.Session.ConversationID == d.ConversationID && (current.Archive == nil || current.Archive.Phase == "" || current.Archive.Phase == "revived") {
				return nil
			}
		} else if code != "agent_unsupported" || nativeMessageSupported(current) {
			return nil
		}
		d.Receipt.Notification.Status = "unavailable"
		d.Receipt.Notification.Retryable = false
		d.Receipt.Notification.Error = accessError(code, "Original native notification target is unavailable; no message was sent.", false)
		return nil
	})
}

// ReconcileAccessV1 observes a saved operation and retries only definitely-unsent
// notifications. It NEVER reissues a backend mutation or replaces a review token.
func (s Service) ReconcileAccessV1(ctx context.Context, taskID, requestID string) (out AccessDecisionResponse) {
	defer func() { out = out.WithOutcome() }()
	out = AccessDecisionResponse{ProtocolVersion: 1, TaskID: taskID, RequestID: requestID}
	t, e := s.authorizedAccessTask(taskID)
	if e != nil {
		out.Error = e
		return out
	}
	if !validAccessID(requestID) {
		out.Error = accessError("invalid_input", "A full request ID is required.", false)
		return out
	}
	release, err := s.Store.taskLease(taskID, "operation")
	if err != nil {
		out.Error = accessError("task_busy", "Task has an active operation; retry reconciliation.", true)
		return out
	}
	t, e = s.authorizedAccessTask(taskID)
	if e != nil {
		release()
		out.Error = e
		return out
	}
	state, err := s.Store.accessState(taskID)
	if err != nil {
		release()
		out.Error = accessError("storage_error", "Cannot read access ledger.", true)
		return out
	}
	r := state.Requests[requestID]
	if r == nil {
		release()
		// A missing task ledger/request is not proof that an earlier intent
		// never existed. Never authorize resubmission based on lost state.
		out.Error = accessError("request_unavailable", "Cannot establish decision absence for an unknown request.", false)
		return out
	}
	id := r.Winner
	if id == "" {
		// No winner does not mean no intent: stale/conflicting attempts have
		// durable receipts too. Return the most recent attempt deterministically.
		var latest time.Time
		for candidate, prior := range state.Decisions {
			if prior.Receipt.RequestID != requestID {
				continue
			}
			created, parseErr := time.Parse(time.RFC3339Nano, prior.Receipt.Decision.CreatedAt)
			if parseErr != nil {
				release()
				out.Error = accessError("storage_error", "Cannot inspect historical access intent.", true)
				return out
			}
			if id == "" || created.After(latest) || (created.Equal(latest) && candidate > id) {
				id, latest = candidate, created
			}
		}
		if id != "" {
			release()
			return s.accessDecisionResponse(taskID, requestID, id)
		}
		// The known request, complete validated ledger, and exclusive operation
		// lease prove no Paddock intent exists. This says nothing about direct
		// upstream administrators; decide still performs fresh preconditions.
		release()
		out.Error = accessError("no_decision", "No Paddock decision intent exists; resubmit the exact frozen decision input and external ID.", true)
		return out
	}
	d := state.Decisions[id]
	if r.View.Kind == "network" {
		obs, refreshErr := s.refreshAccessLocked(ctx, t)
		if refreshErr == nil {
			for _, c := range obs.Chunks {
				if c.ID == requestID {
					d.Receipt.Grant.Status = c.Status
				}
			}
			if d.BackendAcknowledged && d.Receipt.Grant.Status == "approved" {
				if obs.Application.PolicyHash == d.Receipt.Grant.PolicyHash && obs.Application.PolicyVersion == d.Receipt.Grant.PolicyVersion {
					d.Receipt.Grant.ApplicationStatus = obs.Application.Status
				}
			}
		} else {
			d.Receipt.Grant.Error = refreshErr
		}
	}
	if !d.BackendAcknowledged && (d.Receipt.Decision.State == "applying" || d.Receipt.Decision.State == "claimed") {
		d.Receipt.Decision.State = "uncertain"
		d.Receipt.Decision.Error = accessError("outcome_uncertain", "Persisted intent has no acknowledged outcome; backend mutation will not be replayed.", false)
	}
	d.Receipt.Decision.UpdatedAt = now()
	err = s.Store.updateAccessState(taskID, func(v *accessTaskState) error { v.Decisions[id] = d; return nil })
	release()
	if err != nil {
		out.Error = accessError("storage_error", "Could not save reconciliation.", true)
		return out
	}
	s.deliverAccessNotification(ctx, t, id)
	return s.accessDecisionResponse(taskID, requestID, id)
}

// Runs in the existing host runtime poller, including stopped retained tasks.
func (s Service) ReconcileAccessOutboxes(ctx context.Context) error {
	files, err := filepath.Glob(filepath.Join(s.Store.Home, "access-v1", "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".json")
		if !validID(id) {
			continue
		}
		state, err := s.Store.accessState(id)
		if err != nil {
			return err
		}
		t, err := s.Store.Get(id)
		if err != nil {
			return err
		}
		for decisionID, d := range state.Decisions {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !d.BackendAcknowledged && (d.Receipt.Decision.State == "applying" || d.Receipt.Decision.State == "claimed") {
				// A live executor retains the operation lease; reconciliation then
				// returns task_busy. An abandoned durable intent becomes uncertain.
				_ = s.ReconcileAccessV1(ctx, t.ID, d.Receipt.RequestID)
			} else if d.BackendAcknowledged {
				s.deliverAccessNotification(ctx, t, decisionID)
			}
		}
	}
	return nil
}
