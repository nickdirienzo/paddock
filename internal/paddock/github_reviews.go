package paddock

import (
	"fmt"
	"strings"
)

// One submitted review is one durable signal. Keep individual event IDs for
// replies, reactions, and compatibility with comments delivered by older CLIs.
func (g GitHubCoordinator) claimReview(w GitHubWatch, event GitHubEvent, comments []GitHubComment) (GitHubEvent, bool, error) {
	claimed := false
	err := g.Service.Store.UpdateGitHub(func(state *GitHubState) error {
		if _, ok := state.Events[event.ID]; ok {
			return nil
		}
		current, ok := state.Watches[w.Key()]
		if !ok || current.TaskID != w.TaskID {
			return fmt.Errorf("PR watch changed before review delivery")
		}
		allowed := authorizedGitHub(current, event.Comment) && !paddockGeneratedGitHub(event.Identity, event.Comment)
		if allowed {
			for _, c := range comments {
				if c.Kind != "review" || c.ReviewID != event.Comment.ID || c.InReplyTo != 0 || c.User.ID != event.Comment.User.ID || !authorizedGitHub(current, c) || strings.TrimSpace(c.Body) == "" || strings.Contains(c.Body, "<!-- paddock:") {
					continue
				}
				id := githubEventID(w, c)
				if _, exists := state.Events[id]; exists {
					continue
				}
				event.ReviewMembers = append(event.ReviewMembers, id)
				state.Events[id] = GitHubEvent{ID: id, WatchKey: w.Key(), TaskID: w.TaskID, Identity: event.Identity, Comment: c, ReviewBatch: event.ID, Plan: PlanRequest(Request{Body: c.Body}), Status: "claimed", UpdatedAt: now()}
			}
			if len(event.ReviewMembers) > 0 {
				event.Plan = PlanRequest(Request{Body: "Submitted review with inline comments"})
			}
		} else {
			reason := "review author is not authorized"
			if paddockGeneratedGitHub(event.Identity, event.Comment) {
				reason = "Paddock-generated review"
			}
			event.Plan = Plan{Action: "ignore", Reason: reason}
		}
		event.Status, event.UpdatedAt = "claimed", now()
		state.Events[event.ID] = event
		claimed = true
		return nil
	})
	return event, claimed, err
}

func (g GitHubCoordinator) reviewBody(event GitHubEvent) (string, error) {
	state, err := g.Service.Store.GitHubState()
	if err != nil {
		return "", err
	}
	var body strings.Builder
	w := state.Watches[event.WatchKey]
	task, err := g.Service.Store.Get(event.TaskID)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&body, "Submitted GitHub review: %s (%s)\nTreat the summary and inline comments as one review. Read them together, identify shared design concerns, and decide on a coherent response before editing.\n\nReview summary:\n%s\n", event.Comment.HTMLURL, event.Comment.ReviewState, event.Comment.Body)
	for _, id := range event.ReviewMembers {
		member, ok := state.Events[id]
		if !ok {
			return "", fmt.Errorf("review member %s is missing", id)
		}
		if member.ReviewBatch != event.ID || member.TaskID != event.TaskID || member.WatchKey != event.WatchKey || !authorizedGitHub(w, member.Comment) {
			return "", fmt.Errorf("review member no longer authorized")
		}
		c := member.Comment
		fmt.Fprintf(&body, "\nInline comment %d — %s:%d\n%s\n%s\n", c.ID, c.Path, c.Line, c.HTMLURL, c.Body)
		if task.RuntimeName() == "openshell" {
			fmt.Fprintln(&body, "Reply in this inline thread using ordinary gh inside the sandbox.")
		} else {
			fmt.Fprintf(&body, "Reply through: PADDOCK_HOME=%s %s github reply %s %s\n", quote(g.Service.Store.Home), quote(g.Service.Executable), id, quote("Describe the result and validation."))
		}
	}
	return body.String(), nil
}
