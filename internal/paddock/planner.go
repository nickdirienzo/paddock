package paddock

import "strings"

// Request is transport-neutral. Authorization is the caller's responsibility and
// must happen before planning; a plan never grants permission or changes ownership.
type Request struct {
	Body    string `json:"body"`
	Address string `json:"address,omitempty"`
}

type Plan struct {
	Action string `json:"action"`
	Kind   string `json:"kind"`
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason"`
}

// PlanRequest deliberately uses a small, predictable command vocabulary. Freeform
// reasoning belongs to the existing task agent; this router never executes code.
func PlanRequest(r Request) Plan {
	body := strings.TrimSpace(r.Body)
	lower := strings.ToLower(body)
	addressed := false
	prefixes := []string{"paddock:"}
	if r.Address != "" {
		prefixes = append(prefixes, "@"+strings.ToLower(r.Address))
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) && (strings.HasSuffix(prefix, ":") || len(lower) == len(prefix) || strings.ContainsRune(" :\t\n", rune(lower[len(prefix)]))) {
			body = strings.TrimSpace(strings.TrimPrefix(body[len(prefix):], ":"))
			lower = strings.ToLower(body)
			addressed = true
			break
		}
	}
	ignore := Plan{Action: "ignore", Kind: "discussion", Reason: "ordinary discussion or no actionable request"}
	if body == "" {
		return ignore
	}
	if addressed && (lower == "status" || lower == "status?" || lower == "help") {
		return Plan{Action: "reply", Kind: "question", Text: lower, Reason: "local coordination query"}
	}
	for _, prefix := range []string{"delegate:", "separate task:", "create a separate task to ", "please create a separate task to "} {
		if strings.HasPrefix(lower, prefix) {
			text := strings.TrimSpace(body[len(prefix):])
			if text == "" {
				return ignore
			}
			return Plan{Action: "delegate", Kind: "separate_work", Text: text, Reason: "explicit request for independent work"}
		}
	}
	if addressed && strings.HasPrefix(lower, "reply:") {
		text := strings.TrimSpace(body[len("reply:"):])
		if text == "" {
			return ignore
		}
		return Plan{Action: "reply", Kind: "instruction", Text: text, Reason: "explicit reply request"}
	}
	for _, prefix := range []string{"please ", "can you ", "could you ", "would you ", "fix ", "add ", "remove ", "change ", "update ", "rename ", "implement ", "test ", "document "} {
		if strings.HasPrefix(lower, prefix) {
			return Plan{Action: "message", Kind: "instruction", Text: body, Reason: "actionable request for the owning agent"}
		}
	}
	if addressed && strings.Contains(body, "?") {
		return Plan{Action: "message", Kind: "question", Text: body, Reason: "question addressed to the owning agent"}
	}
	if addressed && strings.HasPrefix(lower, "message:") {
		text := strings.TrimSpace(body[len("message:"):])
		if text != "" {
			return Plan{Action: "message", Kind: "instruction", Text: text, Reason: "explicit message request"}
		}
	}
	return ignore
}
