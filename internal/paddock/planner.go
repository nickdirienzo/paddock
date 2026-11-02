package paddock

import "strings"

// Request is transport-neutral. Authorization is the caller's responsibility and
// must happen before planning; a plan never grants permission or changes ownership.
type Request struct {
	Body string `json:"body"`
}

type Plan struct {
	Action string `json:"action"`
	Kind   string `json:"kind"`
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason"`
}

// PlanRequest routes nonempty comments to the watched PR's existing task.
// Authorization and bot filtering belong to the caller; interpretation belongs
// to the task agent, including any mentions or command-like text.
func PlanRequest(r Request) Plan {
	body := strings.TrimSpace(r.Body)
	if body == "" {
		return Plan{Action: "ignore", Kind: "discussion", Reason: "empty comment"}
	}
	return Plan{Action: "message", Kind: "message", Text: body, Reason: "comment on the watched PR"}
}
