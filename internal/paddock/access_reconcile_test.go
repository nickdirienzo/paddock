package paddock

import (
	"context"
	"os"
	"testing"
)

func TestAccessReconcileNoDecisionAllowsExactFrozenResubmission(t *testing.T) {
	s, task, backend, requestID := accessReviewFixture(t)
	in := accessReviewInput(t, s, task, requestID)
	absent := s.ReconcileAccessV1(context.Background(), task.ID, requestID)
	if absent.Error == nil || absent.Error.Code != "no_decision" || !absent.Error.Retryable || absent.Receipt != nil || backend.approveCalls != 0 {
		t.Fatalf("expected authoritative intent absence: %+v", absent)
	}
	accepted := s.DecideAccessV1(context.Background(), requestID, in)
	if accepted.Error != nil || accepted.Receipt == nil || backend.approveCalls != 1 {
		t.Fatalf("frozen decision could not resume after local-only crash: %+v", accepted)
	}
	replay := s.DecideAccessV1(context.Background(), requestID, in)
	if replay.Receipt == nil || replay.Receipt.ID != accepted.Receipt.ID || backend.approveCalls != 1 {
		t.Fatalf("resubmission changed intent identity or executed twice: %+v", replay)
	}
}

func TestAccessReconcileAcceptedIntentNeverMeansNoDecision(t *testing.T) {
	for _, phase := range []string{"claimed", "applying", "uncertain", "confirmed"} {
		t.Run(phase, func(t *testing.T) {
			s, task, backend, requestID := accessReviewFixture(t)
			in := accessReviewInput(t, s, task, requestID)
			accepted := s.DecideAccessV1(context.Background(), requestID, in)
			if accepted.Receipt == nil || accepted.Error != nil {
				t.Fatal(accepted)
			}
			id := accepted.Receipt.ID
			if err := s.Store.updateAccessState(task.ID, func(state *accessTaskState) error {
				d := state.Decisions[id]
				d.Receipt.Decision.State = phase
				d.BackendAcknowledged = phase == "confirmed"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			backend.inspectErr = &OpenShellAccessBackendError{Code: "backend_unavailable", Retryable: true}
			got := s.ReconcileAccessV1(context.Background(), task.ID, requestID)
			if got.Receipt == nil || got.Receipt.ID != id || (got.Error != nil && got.Error.Code == "no_decision") || backend.approveCalls != 1 {
				t.Fatalf("accepted/uncertain intent lost during failed backend inspection: %+v", got)
			}
		})
	}
}

func TestAccessReconcileHistoricalIntentWithoutWinnerReturnsReceipt(t *testing.T) {
	s, task, backend, requestID := accessReviewFixture(t)
	in := accessReviewInput(t, s, task, requestID)
	backend.approveErr = &OpenShellAccessBackendError{Code: "proposal_changed", OutcomeUncertain: false}
	stale := s.DecideAccessV1(context.Background(), requestID, in)
	if stale.Receipt == nil || stale.Receipt.Decision.State != "stale" {
		t.Fatal(stale)
	}
	got := s.ReconcileAccessV1(context.Background(), task.ID, requestID)
	if got.Receipt == nil || got.Receipt.ID != stale.Receipt.ID || got.Error == nil || got.Error.Code == "no_decision" || backend.approveCalls != 1 {
		t.Fatalf("historical intent was mistaken for absence: %+v", got)
	}
}

func TestAccessReconcileUncertainLedgerCannotProveNoDecision(t *testing.T) {
	for _, scenario := range []string{"missing", "malformed", "directory", "dangling_index", "missing_winner", "unknown_request", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			s, task, _, requestID := accessReviewFixture(t)
			in := accessReviewInput(t, s, task, requestID)
			switch scenario {
			case "missing":
				if err := os.Remove(s.Store.accessStatePath(task.ID)); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(s.Store.accessStatePath(task.ID), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(s.Store.accessStatePath(task.ID)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.Store.accessStatePath(task.ID), 0700); err != nil {
					t.Fatal(err)
				}
			case "dangling_index":
				if err := s.Store.updateAccessState(task.ID, func(state *accessTaskState) error { state.Idempotency["unknown-intent-index"] = newID(); return nil }); err != nil {
					t.Fatal(err)
				}
			case "missing_winner":
				accepted := s.DecideAccessV1(context.Background(), requestID, in)
				if accepted.Receipt == nil {
					t.Fatal(accepted)
				}
				if err := s.Store.updateAccessState(task.ID, func(state *accessTaskState) error { state.Requests[requestID].Winner = ""; return nil }); err != nil {
					t.Fatal(err)
				}
			case "unknown_request":
				requestID = "ef5aaeb0-8a88-4430-be71-09eccbb10b19"
			case "busy":
				release, err := s.Store.taskLease(task.ID, "operation")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			got := s.ReconcileAccessV1(context.Background(), task.ID, requestID)
			if got.Error == nil || got.Error.Code == "no_decision" || got.Receipt != nil {
				t.Fatalf("ambiguous/unavailable ledger incorrectly permits resubmission: %+v", got)
			}
		})
	}
}
