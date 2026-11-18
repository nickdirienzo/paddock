package paddock

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// AccessReview is transport-neutral review text. Integrations render it as plain
// text without rebuilding runtime policy semantics or shortening permission scope.
type AccessReview struct {
	Summary string   `json:"summary"`
	Details []string `json:"details"`
}

// AccessOutcome is the integration's recovery disposition, derived from the
// authoritative decision and notification receipts rather than persisted twice.
type AccessOutcome struct {
	State   string `json:"state"` // unresolved, settled, review_ended, not_submitted
	Summary string `json:"summary"`
}

func receiptOutcome(r AccessReceipt) AccessOutcome {
	parts := []string{"Decision: " + r.Decision.Value + " (" + r.Decision.State + ")", "Access: " + r.Grant.Status, "Policy: " + r.Grant.ApplicationStatus, "Agent notification: " + r.Notification.Status}
	for _, e := range []*AccessError{r.Decision.Error, r.Grant.Error, r.Notification.Error} {
		if e != nil {
			parts = append(parts, e.Message)
		}
	}
	out := AccessOutcome{State: "unresolved", Summary: strings.Join(parts, "; ")}
	if r.Decision.State == "stale" || r.Decision.State == "conflict" {
		out.State = "review_ended"
	} else if r.Decision.State == "confirmed" && slices.Contains([]string{"approved", "rejected", "withdrawn", "superseded"}, r.Grant.Status) && slices.Contains([]string{"loaded", "error", "unknown", "not_applicable"}, r.Grant.ApplicationStatus) && slices.Contains([]string{"delivered", "unavailable", "uncertain", "not_requested"}, r.Notification.Status) {
		out.State = "settled"
	}
	return out
}

// WithOutcome also covers structured CLI failures before admission. A failed
// inspection alone cannot establish that resubmission is safe.
func (r AccessDecisionResponse) WithOutcome() AccessDecisionResponse {
	out := AccessOutcome{State: "unresolved", Summary: "Paddock has not confirmed an access decision."}
	if r.Receipt != nil {
		out = receiptOutcome(*r.Receipt)
		if r.Error != nil && (r.Error.Code == "storage_error" || r.Error.Code == "outcome_uncertain") {
			out.State = "unresolved"
			out.Summary += "; " + r.Error.Message
		}
	} else if r.Error != nil {
		out.Summary = r.Error.Message
		if r.Error.Code == "no_decision" && r.Error.Retryable {
			out.State = "not_submitted"
		}
	}
	r.Outcome = &out
	return r
}

func presentAccessView(v AccessView) AccessView {
	review := AccessReview{Summary: "Network access requested", Details: []string{}}
	outcome := AccessOutcome{State: "unresolved", Summary: "Request: " + v.Status}
	if v.Receipt != nil {
		outcome = receiptOutcome(*v.Receipt)
	} else if v.Error != nil {
		outcome.Summary = "Access unavailable: " + v.Error.Message
	}
	if v.Snapshot == nil {
		v.Actionable = false
		v.SupportedDecisions = []string{}
		outcome.Summary = "This request cannot be approved here; an operator must inspect it through Paddock."
	} else {
		s := v.Snapshot
		review.Details = []string{"Task: " + v.TaskID, "Sandbox: " + v.Target.SandboxName + " (" + v.Target.SandboxID + ")", "Gateway: " + v.Target.Gateway + "; workspace: " + v.Target.Workspace}
		if s.Rationale != "" {
			review.Details = append(review.Details, "Purpose: "+s.Rationale)
		}
		if s.RuleName != "" {
			review.Details = append(review.Details, "Rule name: "+s.RuleName)
		}
		review.Details = append(review.Details, "Scope: this sandbox. This rule may permit later matching requests; it is not a single download.")
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, s.Rule, "", "  "); err != nil {
			v.Actionable = false
			v.SupportedDecisions = []string{}
			outcome.Summary = "The complete reviewed rule is unavailable."
		} else {
			review.Details = append(review.Details, "Requested access:\n"+pretty.String())
		}
		if s.ValidationResult != "" {
			review.Details = append(review.Details, "Validation: "+s.ValidationResult)
		}
		if len(s.Security) > 0 {
			review.Details = append(review.Details, "Security findings:\n"+strings.Join(s.Security, "\n"))
		}
		if s.ApplicationError != "" {
			review.Details = append(review.Details, "Application: "+s.ApplicationError)
		}
		if s.CandidateEffectivePolicyHash != "" {
			review.Details = append(review.Details, "Evaluated candidate policy: "+s.CandidateEffectivePolicyHash)
		}
		if v.Actionable && slices.Contains(v.SupportedDecisions, "reject") && v.DecisionSemantics.Reject == "current_pending_request" {
			review.Details = append(review.Details, "Reject declines this pending request; its backend does not bind rejection to an exact rule revision.")
		}
	}
	v.Review, v.Outcome = &review, &outcome
	return v
}
