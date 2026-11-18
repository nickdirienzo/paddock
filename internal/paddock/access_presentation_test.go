package paddock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAccessOutcomeOwnsRecoveryDisposition(t *testing.T) {
	base := AccessReceipt{Decision: AccessDecisionState{Value: "approve", State: "confirmed"}, Grant: AccessGrant{Status: "approved", ApplicationStatus: "loaded"}, Notification: AccessNotification{Status: "delivered"}}
	for _, tc := range []struct {
		name, state string
		mutate      func(*AccessReceipt)
	}{
		{"applied and delivered", "settled", func(*AccessReceipt) {}},
		{"application status missing", "unresolved", func(r *AccessReceipt) { r.Grant.ApplicationStatus = "" }},
		{"future application status", "unresolved", func(r *AccessReceipt) { r.Grant.ApplicationStatus = "reloading" }},
		{"policy pending", "unresolved", func(r *AccessReceipt) { r.Grant.ApplicationStatus = "pending" }},
		{"delivery pending", "unresolved", func(r *AccessReceipt) { r.Notification.Status = "pending" }},
		{"delivery claimed", "unresolved", func(r *AccessReceipt) { r.Notification.Status = "claimed" }},
		{"delivery uncertain requires inspection", "settled", func(r *AccessReceipt) { r.Notification.Status = "uncertain" }},
		{"mutation uncertain cannot replay", "unresolved", func(r *AccessReceipt) { r.Decision.State = "uncertain" }},
		{"stale review", "review_ended", func(r *AccessReceipt) { r.Decision.State = "stale" }},
		{"conflicting review", "review_ended", func(r *AccessReceipt) { r.Decision.State = "conflict" }},
		{"unknown future state", "unresolved", func(r *AccessReceipt) { r.Decision.State = "new-state" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			got := (AccessDecisionResponse{Receipt: &r}).WithOutcome()
			if got.Outcome == nil || got.Outcome.State != tc.state || !strings.Contains(got.Outcome.Summary, r.Notification.Status) {
				t.Fatal(got)
			}
		})
	}
	for _, code := range []string{"no_decision", "storage_error", "request_unavailable", "outcome_uncertain"} {
		got := (AccessDecisionResponse{Error: accessError(code, "Inspect this result", true)}).WithOutcome()
		want := "unresolved"
		if code == "no_decision" {
			want = "not_submitted"
		}
		if got.Outcome.State != want {
			t.Fatal(code, got)
		}
	}
}

func TestAccessReviewPreservesFullScopeAndPrecision(t *testing.T) {
	rule := json.RawMessage(`{"name":"large-number","opaque_parameter":9223372036854775807,"endpoints":[{"host":"registry.example","port":443}]}`)
	v := presentAccessView(AccessView{TaskID: "task", Status: "pending", Target: AccessTarget{SandboxID: "sandbox", SandboxName: "name"}, Snapshot: &AccessSnapshot{Rule: rule, RuleName: "large-number", Rationale: "literal <@USER>"}})
	text := strings.Join(v.Review.Details, "\n")
	for _, want := range []string{"9223372036854775807", "registry.example", "443", "large-number", "literal <@USER>", "not a single download"} {
		if !strings.Contains(text, want) {
			t.Fatal("review omitted scope", want, text)
		}
	}
}

func TestAccessOutcomeRequiresDurableConfirmation(t *testing.T) {
	receipt := AccessReceipt{Decision: AccessDecisionState{Value: "approve", State: "confirmed"}, Grant: AccessGrant{Status: "approved", ApplicationStatus: "loaded"}, Notification: AccessNotification{Status: "unavailable"}}
	for _, code := range []string{"storage_error", "outcome_uncertain"} {
		got := (AccessDecisionResponse{Receipt: &receipt, Error: accessError(code, "Durable receipt is unconfirmed", false)}).WithOutcome()
		if got.Outcome.State != "unresolved" || !strings.Contains(got.Outcome.Summary, "Durable receipt is unconfirmed") {
			t.Fatal(code, got)
		}
	}
}

func TestAccessExportedPresentationsMatchBackend(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "access-wire-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Examples map[string]json.RawMessage `json:"exported_examples"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"list", "show"} {
		var views []AccessView
		if name == "list" {
			var r AccessListResponse
			if err = json.Unmarshal(doc.Examples[name], &r); err != nil {
				t.Fatal(err)
			}
			views = r.Requests
		} else {
			var r AccessShowResponse
			if err = json.Unmarshal(doc.Examples[name], &r); err != nil || r.Request == nil {
				t.Fatal(err)
			}
			views = []AccessView{*r.Request}
		}
		for _, v := range views {
			actual := presentAccessView(v)
			if !reflect.DeepEqual(v.Review, actual.Review) || !reflect.DeepEqual(v.Outcome, actual.Outcome) {
				t.Fatalf("%s presentation drift: %#v vs %#v", name, v, actual)
			}
		}
	}
	for _, name := range []string{"decision_response", "uncertain_response", "no_decision_response"} {
		var r AccessDecisionResponse
		if err = json.Unmarshal(doc.Examples[name], &r); err != nil {
			t.Fatal(err)
		}
		actual := r.WithOutcome()
		if !reflect.DeepEqual(r.Outcome, actual.Outcome) {
			t.Fatalf("%s outcome drift: %+v vs %+v", name, r.Outcome, actual.Outcome)
		}
	}
}
