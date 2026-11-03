package paddock

import (
	"context"
	"fmt"
	"slices"
)

func (g GitHubCoordinator) ConfigureRouting(ctx context.Context, raw, mode, model string, tasks, repos []string) error {
	repo, n, err := ParsePullRequest(raw)
	if err != nil {
		return err
	}
	if mode != "planner" && mode != "direct" {
		return fmt.Errorf("routing mode must be planner or direct")
	}
	state, err := g.Service.Store.GitHubState()
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s#%d", repo, n)
	w, ok := state.Watches[key]
	if !ok {
		return fmt.Errorf("register a watch first")
	}
	policy := OrchestrationPolicy{}
	if mode == "planner" {
		policy, err = g.Service.ResolveOrchestrationPolicy(ctx, w.TaskID, tasks, repos)
		if err != nil {
			return err
		}
	} else if len(tasks) > 0 || len(repos) > 0 || model != "" {
		return fmt.Errorf("direct routing has no planner scope or model")
	}
	return g.Service.Store.UpdateGitHub(func(state *GitHubState) error {
		current, ok := state.Watches[key]
		if !ok || current.TaskID != w.TaskID {
			return fmt.Errorf("watch changed")
		}
		current.Routing = mode
		current.Orchestration = policy
		current.PlannerModel = model
		state.Watches[key] = current
		return nil
	})
}

func (g GitHubCoordinator) routeSignal(ctx context.Context, w GitHubWatch, event GitHubEvent) (GitHubEvent, error) {
	mode := "direct"
	policy := OrchestrationPolicy{Tasks: []string{w.TaskID}}
	planner := g.Planner
	if w.Routing == "planner" {
		mode = "planner"
		policy = w.Orchestration
		if planner == nil {
			config, err := g.Service.Store.PlannerConfig()
			if err != nil {
				return event, err
			}
			if w.PlannerModel != "" {
				config.Model = w.PlannerModel
			}
			planner = CodexPlanner{Model: config.Model, Instructions: config.Instructions}
		}
	}
	signal := Signal{Source: "github", ID: event.ID, Body: event.Comment.Body, TaskID: w.TaskID}
	if event.Comment.Kind == "review_submission" {
		var err error
		signal.Body, err = g.reviewBody(event)
		if err != nil {
			return event, err
		}
	}
	orchestrator := Orchestrator{Service: g.Service, Planner: planner, GitHub: &g}
	record, err := orchestrator.Process(ctx, signal, OrchestrationOptions{Mode: mode, Policy: policy})
	event.SignalKey = signal.Key()
	event.Error = record.Error
	if err != nil {
		event.Error = err.Error()
	}
	event.Status = record.Status
	if event.Status != "done" && event.Status != "uncertain" {
		event.Status = "pending"
	}
	// Preserve the established event summary. Full decisions and execution results
	// live in signals/<signal-key>.json and never feed the legacy delivery path.
	if len(record.Outcomes) > 0 {
		first := record.Outcomes[0].Action
		event.Plan = Plan{Action: first.Kind, Kind: first.Kind, Text: first.Text, Reason: first.Reason}
		if len(record.Outcomes) > 1 {
			event.Plan.Action = "orchestrate"
		}
	}
	event.Result = ""
	for _, out := range record.Outcomes {
		if out.Status != "done" {
			continue
		}
		if out.Action.Kind == "message" {
			event.Result = "Queued for agent session " + out.Result
			event.Receipt = "pending"
		}
	}
	if e := g.saveEvent(event); e != nil {
		return event, e
	}
	if record.Status == "uncertain" {
		return event, nil
	}
	return event, err
}

// Pin reply authority before delivery, so a fast recipient can immediately enqueue
// its result. This grants access to this one event only, never to another watch.
func (g GitHubCoordinator) grantSignalRecipient(eventID, taskID string) error {
	return g.Service.Store.UpdateGitHub(func(state *GitHubState) error {
		e, ok := state.Events[eventID]
		if !ok {
			return fmt.Errorf("event not found")
		}
		w, ok := state.Watches[e.WatchKey]
		if !ok || !authorizedGitHub(w, e.Comment) {
			return fmt.Errorf("event no longer authorized")
		}
		for _, id := range append([]string{eventID}, e.ReviewMembers...) {
			member, ok := state.Events[id]
			if !ok || !authorizedGitHub(w, member.Comment) {
				return fmt.Errorf("review member no longer authorized")
			}
			if !slices.Contains(member.RecipientTaskIDs, taskID) {
				member.RecipientTaskIDs = append(member.RecipientTaskIDs, taskID)
			}
			state.Events[id] = member
		}
		return nil
	})
}
