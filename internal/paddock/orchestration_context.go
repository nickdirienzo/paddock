package paddock

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

type TaskSummary struct {
	ID           string `json:"id"`
	ParentID     string `json:"parent_id,omitempty"`
	Repo         string `json:"repo"`
	Prompt       string `json:"prompt"`
	Status       string `json:"status"`
	Agent        string `json:"agent"`
	SessionState string `json:"session_state"`
	Branch       string `json:"branch"`
	BaseCommit   string `json:"base_commit"`
}
type PRSummary struct {
	Key    string `json:"key"`
	TaskID string `json:"task_id"`
	Head   string `json:"head"`
}
type PlanningInput struct {
	Signal  Signal              `json:"signal"`
	Policy  OrchestrationPolicy `json:"policy"`
	Tasks   []TaskSummary       `json:"tasks"`
	PRs     []PRSummary         `json:"prs"`
	Context []ContextEvidence   `json:"context"`
}
type ContextEvidence struct {
	Request ContextRequest `json:"request"`
	Data    any            `json:"data,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func bounded(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n[truncated]"
	}
	return s
}
func (o Orchestrator) perceive(ctx context.Context, signal Signal, p OrchestrationPolicy) (PlanningInput, error) {
	input := PlanningInput{Signal: signal, Policy: p, Tasks: []TaskSummary{}, PRs: []PRSummary{}, Context: []ContextEvidence{}}
	for _, id := range p.Tasks {
		t, err := o.Service.Store.Get(id)
		if err != nil {
			return input, err
		}
		input.Tasks = append(input.Tasks, TaskSummary{ID: t.ID, ParentID: t.ParentID, Repo: t.Repo, Prompt: bounded(t.Prompt, 2000), Status: t.Status, Agent: t.Session.Agent, SessionState: plannerSessionState(ctx, t), Branch: t.Branch, BaseCommit: t.BaseCommit})
	}
	state, err := o.Service.Store.GitHubState()
	if err != nil {
		return input, err
	}
	for key, w := range state.Watches {
		if slices.Contains(p.Tasks, w.TaskID) {
			input.PRs = append(input.PRs, PRSummary{Key: key, TaskID: w.TaskID, Head: w.HeadRef})
		}
	}
	slices.SortFunc(input.PRs, func(a, b PRSummary) int { return strings.Compare(a.Key, b.Key) })
	return input, nil
}

// Only these brokered, read-only operations are available to the planner. It
// cannot supply shell commands, file paths, API endpoints, or credentials.
func (o Orchestrator) readContext(ctx context.Context, signal Signal, p OrchestrationPolicy, request ContextRequest) ContextEvidence {
	e := ContextEvidence{Request: request}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	read := func() (any, error) {
		switch request.Kind {
		case "task", "session":
			if !slices.Contains(p.Tasks, request.ID) {
				return nil, fmt.Errorf("task is outside authorized scope")
			}
			task, err := o.Service.Store.Get(request.ID)
			if err != nil {
				return nil, err
			}
			if request.Kind == "task" {
				return map[string]any{"id": task.ID, "parent_id": task.ParentID, "prompt": bounded(task.Prompt, 16000), "repo": task.Repo, "branch": task.Branch, "status": task.Status, "agent": task.Session.Agent, "conversation_id": task.Session.ConversationID, "error": task.Error}, nil
			}
			if task.Session.Backend != "rex" {
				return nil, fmt.Errorf("session capture currently requires Rex; task metadata is still available")
			}
			session, obs := terminalFor(task.Session).Inspect(ctx, task)
			if obs.State != "running" || session.BlockID == "" {
				return nil, fmt.Errorf("session is unavailable: %s", obs.State)
			}
			data, err := (terminal{backend: session.Backend, path: session.TerminalPath, endpoint: session.Endpoint}).rex(ctx, "capture", "-s", session.ID, "-b", session.BlockID, "--trim", "--unwrap")
			if len(data) > 16000 {
				data = data[len(data)-16000:]
			}
			return string(data), err
		case "repo":
			if !slices.Contains(p.Repos, request.ID) {
				return nil, fmt.Errorf("repository is outside authorized scope")
			}
			// These fixed Git invocations neither refresh remotes nor run user hooks.
			log, err := git(ctx, request.ID, "log", "-5", "--format=%h %s")
			if err != nil {
				return nil, err
			}
			files, err := git(ctx, request.ID, "ls-tree", "--name-only", "HEAD")
			if err != nil {
				return nil, err
			}
			readme, _ := git(ctx, request.ID, "show", "HEAD:README.md")
			return map[string]string{"recent_commits": bounded(log, 4000), "files": bounded(files, 4000), "readme": bounded(readme, 12000)}, nil
		case "pr":
			state, err := o.Service.Store.GitHubState()
			if err != nil {
				return nil, err
			}
			w, ok := state.Watches[request.ID]
			if !ok || !slices.Contains(p.Tasks, w.TaskID) {
				return nil, fmt.Errorf("PR is outside authorized scope")
			}
			g := o.GitHub
			if g == nil {
				v := NewGitHubCoordinator(o.Service)
				g = &v
			}
			api, _, err := g.remoteFor(state, w.Repo, nil)
			if err != nil {
				return nil, err
			}
			if err = api.ValidateIdentity(ctx); err != nil {
				return nil, err
			}
			pr, err := api.Pull(ctx, w.Repo, w.Number)
			if err != nil {
				return nil, err
			}
			comments, err := api.Comments(ctx, w.Repo, w.Number)
			if err != nil {
				return nil, err
			}
			if len(comments) > 20 {
				comments = comments[len(comments)-20:]
			}
			for i := range comments {
				comments[i].Body = bounded(comments[i].Body, 2000)
			}
			pr.Body = bounded(pr.Body, 6000)
			return map[string]any{"pull_request": pr, "recent_comments": comments}, nil
		}
		return nil, fmt.Errorf("unknown context kind %q", request.Kind)
	}
	data, err := read()
	if err != nil {
		e.Error = err.Error()
	} else {
		e.Data = data
	}
	return e
}

// Perception is read-only even for previews: do not refresh persisted task status.
func plannerSessionState(ctx context.Context, t Task) string {
	if t.Status != "running" {
		return "offline"
	}
	_, obs := terminalFor(t.Session).Inspect(ctx, t)
	if obs.State == "running" {
		return "online"
	}
	if obs.State == "unknown" {
		return "unknown"
	}
	return "offline"
}
