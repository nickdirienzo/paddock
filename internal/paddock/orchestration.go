package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Signal is independent of its transport. Source + ID is its immutable idempotency key.
type Signal struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Body   string `json:"body"`
	TaskID string `json:"task_id,omitempty"`
}

type OrchestrationPolicy struct {
	Tasks []string `json:"tasks"`
	Repos []string `json:"repos"`
}

type Action struct {
	Kind   string `json:"kind"`
	TaskID string `json:"task_id"`
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Agent  string `json:"agent"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}
type ContextRequest struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type Decision struct {
	Requests []ContextRequest `json:"requests"`
	Actions  []Action         `json:"actions"`
}
type ActionOutcome struct {
	Action Action `json:"action"`
	Status string `json:"status"`
	TaskID string `json:"created_task_id,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
type SignalRecord struct {
	PlannerConfig *PlannerConfig      `json:"planner_config,omitempty"`
	Version       int                 `json:"version"`
	Signal        Signal              `json:"signal"`
	Policy        OrchestrationPolicy `json:"policy"`
	Mode          string              `json:"mode"`
	Planner       string              `json:"planner,omitempty"`
	Status        string              `json:"status"`
	Rounds        []Decision          `json:"rounds"`
	Context       []ContextEvidence   `json:"context"`
	Outcomes      []ActionOutcome     `json:"outcomes"`
	Error         string              `json:"error,omitempty"`
	UpdatedAt     string              `json:"updated_at"`
}
type OrchestrationOptions struct {
	DryRun bool
	Mode   string
	Policy OrchestrationPolicy
}
type DecisionPlanner interface {
	Decide(context.Context, PlanningInput) (Decision, error)
	Name() string
}
type Orchestrator struct {
	Service Service
	Planner DecisionPlanner
	GitHub  *GitHubCoordinator
}

func (s Signal) Key() string { return githubID(s.Source + "\x00" + s.ID) }
func (s *Store) SignalRecord(source, id string) (SignalRecord, error) {
	var r SignalRecord
	b, err := os.ReadFile(filepath.Join(s.Home, "signals", (Signal{Source: source, ID: id}).Key()+".json"))
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err == nil && (r.Version != 1 || r.Signal.Source != source || r.Signal.ID != id) {
		err = fmt.Errorf("invalid signal record")
	}
	return r, err
}
func (o Orchestrator) saveSignal(r *SignalRecord) error {
	r.UpdatedAt = now()
	return writeJSONAtomic(filepath.Join(o.Service.Store.Home, "signals", r.Signal.Key()+".json"), r)
}

// Scope is supplied by the operator, never by the planner or comment text.
// Canonical paths and full task IDs pin aliases before the model sees them.
func (s Service) ResolveOrchestrationPolicy(ctx context.Context, origin string, tasks, repos []string) (OrchestrationPolicy, error) {
	p := OrchestrationPolicy{Tasks: []string{}, Repos: []string{}}
	if origin != "" {
		tasks = append(append([]string{}, tasks...), origin)
	}
	for _, id := range tasks {
		t, err := s.Store.Get(id)
		if err != nil {
			return p, err
		}
		if !slices.Contains(p.Tasks, t.ID) {
			p.Tasks = append(p.Tasks, t.ID)
		}
		if id == origin {
			repos = append(append([]string{}, repos...), t.Repo)
		}
	}
	for _, name := range repos {
		path, err := ResolveRepoArgument(s.Store.Home, name)
		if err != nil {
			return p, err
		}
		r, err := discoverRepository(ctx, path)
		if err != nil {
			return p, err
		}
		if !slices.Contains(p.Repos, r.Root) {
			p.Repos = append(p.Repos, r.Root)
		}
	}
	slices.Sort(p.Tasks)
	slices.Sort(p.Repos)
	return p, nil
}

func (o Orchestrator) authorize(signal Signal, policy OrchestrationPolicy) error {
	if signal.Source == "" || signal.ID == "" || len(signal.ID) > 512 || len(signal.Body) > 64000 {
		return fmt.Errorf("invalid signal source, ID, or body length")
	}
	if signal.Source == "github" {
		if o.GitHub == nil {
			return fmt.Errorf("GitHub signals require their authenticated adapter")
		}
		state, err := o.Service.Store.GitHubState()
		if err != nil {
			return err
		}
		e, ok := state.Events[signal.ID]
		if !ok {
			return fmt.Errorf("unknown GitHub event")
		}
		w, ok := state.Watches[e.WatchKey]
		body := e.Comment.Body
		if e.Comment.Kind == "review_submission" {
			body, err = o.GitHub.reviewBody(e)
			if err != nil {
				return err
			}
		}
		if !ok || w.TaskID != signal.TaskID || e.TaskID != signal.TaskID || body != signal.Body || !authorizedGitHub(w, e.Comment) || paddockGeneratedGitHub(e.Identity, e.Comment) {
			return fmt.Errorf("GitHub signal is no longer authorized")
		}
		expected := w.Orchestration
		if w.Routing != "planner" {
			expected = OrchestrationPolicy{Tasks: []string{w.TaskID}}
		}
		// Compare canonical scope set by watch configuration; a source cannot expand it.
		if !slices.Equal(policy.Tasks, expected.Tasks) || !slices.Equal(policy.Repos, expected.Repos) {
			return fmt.Errorf("GitHub orchestration scope changed")
		}
	}
	return nil
}

// Process implements Signal -> Perceive -> Plan -> Act -> Exit. The per-signal
// lease spans planning and execution, while ordinary task writes stay unlocked.
func (o Orchestrator) Process(ctx context.Context, signal Signal, opts OrchestrationOptions) (SignalRecord, error) {
	r := SignalRecord{Version: 1, Signal: signal, Policy: opts.Policy, Mode: opts.Mode, Status: "planning", Rounds: []Decision{}, Context: []ContextEvidence{}, Outcomes: []ActionOutcome{}}
	if r.Mode == "" {
		r.Mode = "planner"
	}
	if r.Mode != "planner" && r.Mode != "direct" {
		return r, fmt.Errorf("mode must be planner or direct")
	}
	if err := o.authorize(signal, opts.Policy); err != nil {
		return r, err
	}
	dir := filepath.Join(o.Service.Store.Home, "signals")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return r, err
	}
	f, err := os.OpenFile(filepath.Join(dir, signal.Key()+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return r, err
	}
	defer f.Close()
	locked, err := tryLock(f)
	if err != nil {
		return r, err
	}
	if !locked {
		return r, fmt.Errorf("signal is already being processed")
	}
	defer unlock(f)
	old, err := o.Service.Store.SignalRecord(signal.Source, signal.ID)
	if err == nil {
		if old.Signal != signal {
			return old, fmt.Errorf("signal ID already belongs to different content")
		}
		if old.Status == "done" || old.Status == "uncertain" {
			return old, nil
		}
		for i := range old.Outcomes {
			if old.Outcomes[i].Status == "executing" {
				old.Outcomes[i].Status = "uncertain"
				old.Outcomes[i].Error = "process ended during an action; inspect its destination before any retry"
				old.Status = "uncertain"
			}
		}
		if old.Status == "uncertain" {
			return old, o.saveSignal(&old)
		}
		// Dry runs are inspectable, but never consume a signal. A later live call
		// perceives and plans afresh, rather than treating preview as approval.
		if old.Status != "dry_run" && len(old.Outcomes) > 0 {
			r = old
			r.Policy = opts.Policy
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return r, err
	}
	fail := func(err error) (SignalRecord, error) {
		r.Error = err.Error()
		if len(r.Outcomes) == 0 {
			r.Status = "planning_failed"
		} else if r.Status != "uncertain" {
			r.Status = "pending"
		}
		if e := o.saveSignal(&r); e != nil {
			return r, e
		}
		return r, err
	}
	if len(r.Outcomes) == 0 {
		if err := o.saveSignal(&r); err != nil {
			return r, err
		}
		var decision Decision
		if r.Mode == "direct" {
			p := PlanRequest(Request{Body: signal.Body})
			decision = Decision{Requests: []ContextRequest{}, Actions: []Action{{Kind: p.Action, TaskID: signal.TaskID, Text: p.Text, Reason: p.Reason}}}
			if p.Action == "ignore" {
				decision.Actions[0].TaskID = ""
			}
		} else {
			planner := o.Planner
			if planner == nil {
				config, err := o.Service.Store.PlannerConfig()
				if err != nil {
					return fail(err)
				}
				planner = CodexPlanner{Model: config.Model, Instructions: config.Instructions}
			}
			r.Planner = planner.Name()
			if p, ok := planner.(CodexPlanner); ok {
				r.PlannerConfig = &PlannerConfig{Model: p.Model, Instructions: p.Instructions}
			}
			input, err := o.perceive(ctx, signal, opts.Policy)
			if err != nil {
				return fail(err)
			}
			for round := 0; round < 5; round++ {
				input.Context = r.Context
				decision, err = planner.Decide(ctx, input)
				if err != nil {
					return fail(err)
				}
				r.Rounds = append(r.Rounds, decision)
				if len(decision.Requests) == 0 {
					break
				}
				if len(decision.Actions) > 0 || len(decision.Requests) > 6 || round == 4 {
					return fail(fmt.Errorf("planner must request at most six context reads or finish with actions within five rounds"))
				}
				for _, request := range decision.Requests {
					evidence := o.readContext(ctx, signal, opts.Policy, request)
					r.Context = append(r.Context, evidence)
				}
				if err = o.saveSignal(&r); err != nil {
					return r, err
				}
			}
		}
		if len(r.Rounds) == 0 {
			r.Rounds = append(r.Rounds, decision)
		}
		if err = o.validateActions(ctx, signal, opts.Policy, decision.Actions); err != nil {
			return fail(err)
		}
		for _, action := range decision.Actions {
			r.Outcomes = append(r.Outcomes, ActionOutcome{Action: action, Status: "pending"})
		}
		r.Status = "planned"
		r.Error = ""
		if opts.DryRun {
			r.Status = "dry_run"
		}
		if err = o.saveSignal(&r); err != nil {
			return r, err
		}
	}
	if opts.DryRun {
		return r, nil
	}
	actions := []Action{}
	for _, out := range r.Outcomes {
		actions = append(actions, out.Action)
	}
	if err = o.validateActions(ctx, signal, opts.Policy, actions); err != nil {
		return fail(err)
	}
	for i := range r.Outcomes {
		out := &r.Outcomes[i]
		if out.Status == "done" {
			continue
		}
		if err = o.authorize(signal, opts.Policy); err != nil {
			return fail(err)
		}
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if out.Action.Kind == "message" && !o.Service.CanDeliver(ctx, out.Action.TaskID) {
			r.Status = "pending"
			out.Error = "destination agent is offline or unconfirmed"
			return r, o.saveSignal(&r)
		}
		if out.Action.Kind == "delegate" || out.Action.Kind == "create_task" {
			out.TaskID = newID()
		}
		out.Status = "executing"
		out.Error = ""
		r.Status = "executing"
		if err = o.saveSignal(&r); err != nil {
			return r, err
		}
		result, err := o.act(ctx, signal, *out)
		out.Result = result
		if errors.Is(err, ErrAgentOffline) {
			out.Status = "pending"
			r.Status = "pending"
			out.Error = err.Error()
			return r, o.saveSignal(&r)
		}
		if err != nil {
			out.Status = "uncertain"
			r.Status = "uncertain"
			out.Error = err.Error()
			return fail(err)
		}
		out.Status = "done"
		if err = o.saveSignal(&r); err != nil {
			return r, err
		}
	}
	r.Status = "done"
	r.Error = ""
	return r, o.saveSignal(&r)
}

func (o Orchestrator) validateActions(ctx context.Context, signal Signal, p OrchestrationPolicy, actions []Action) error {
	if len(actions) < 1 || len(actions) > 4 {
		return fmt.Errorf("planner must choose one to four actions")
	}
	seen := map[string]bool{}
	for _, a := range actions {
		if strings.TrimSpace(a.Reason) == "" || len(a.Text) > 32000 {
			return fmt.Errorf("action needs rationale and bounded text")
		}
		identity := a
		identity.Reason = ""
		if a.Kind == "message" {
			identity.Text = ""
		}
		data, _ := json.Marshal(identity)
		key := string(data)
		if seen[key] {
			return fmt.Errorf("duplicate action")
		}
		seen[key] = true
		if a.Kind != "ignore" && strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("action text is empty")
		}
		switch a.Kind {
		case "ignore":
			if len(actions) != 1 || a.TaskID != "" || a.Repo != "" || a.Base != "" || a.Agent != "" || a.Text != "" {
				return fmt.Errorf("ignore must be the only action and have no target or text")
			}
		case "message", "delegate":
			if !slices.Contains(p.Tasks, a.TaskID) {
				return fmt.Errorf("task %s is outside authorized scope", a.TaskID)
			}
			task, err := o.Service.Store.Get(a.TaskID)
			if err != nil {
				return err
			}
			if a.Repo != "" {
				return fmt.Errorf("task actions derive repository from their target")
			}
			if a.Kind == "message" {
				if !nativeMessageSupported(task) || a.Base != "" || a.Agent != "" {
					return fmt.Errorf("message requires a task with native delivery support and no launch options")
				}
			} else {
				if !slices.Contains(p.Repos, task.Repo) {
					return fmt.Errorf("child task repository is outside authorized scope")
				}
				if a.Base != "" && a.Base != task.Branch && a.Base != task.BaseCommit {
					return fmt.Errorf("child base must be the parent branch or base commit")
				}
				if a.Agent != "codex" && a.Agent != "claude" {
					return fmt.Errorf("choose codex or claude for child agent")
				}
			}
		case "create_task":
			if a.TaskID != "" || a.Base != "" || !slices.Contains(p.Repos, a.Repo) {
				return fmt.Errorf("independent work requires an authorized repository and its default base")
			}
			if a.Agent != "codex" && a.Agent != "claude" {
				return fmt.Errorf("choose codex or claude for new agent")
			}
		case "reply":
			if a.TaskID != "" || a.Repo != "" || a.Base != "" || a.Agent != "" {
				return fmt.Errorf("reply goes to the originating signal, not an arbitrary target")
			}
		default:
			return fmt.Errorf("unknown action %q", a.Kind)
		}
	}
	return nil
}
func (o Orchestrator) act(ctx context.Context, signal Signal, out ActionOutcome) (string, error) {
	a := out.Action
	switch a.Kind {
	case "ignore":
		return a.Reason, nil
	case "reply":
		if signal.Source == "github" {
			post, err := o.GitHub.EnqueueReply(signal.ID, a.Text)
			return "Queued reply " + post.ID, err
		}
		return a.Text, nil
	case "message":
		task, err := o.Service.Store.Get(a.TaskID)
		if err != nil {
			return "", err
		}
		text := a.Text
		if signal.Source == "github" {
			if err := o.GitHub.grantSignalRecipient(signal.ID, a.TaskID); err != nil {
				return "", err
			}
			state, err := o.Service.Store.GitHubState()
			if err != nil {
				return "", err
			}
			e := state.Events[signal.ID]
			reply := fmt.Sprintf("Reply through: PADDOCK_HOME=%s %s github reply %s %s", quote(o.Service.Store.Home), quote(o.Service.Executable), signal.ID, quote("Describe the result and validation."))
			if task.RuntimeName() == "openshell" {
				reply = "Reply on this PR using ordinary gh inside this sandbox, describing the result and validation. For a review-thread comment, reply in that review thread. The host owns Paddock coordination."
			}
			text = fmt.Sprintf("GitHub comment from %s on %s\nReview context: %s:%d\nOrigin task: %s\n\n%s\n\n%s", e.Comment.User.Login, e.Comment.HTMLURL, e.Comment.Path, e.Comment.Line, signal.TaskID, text, reply)
		}
		return o.Service.Message(ctx, task, text)
	case "delegate", "create_task":
		parent := ""
		repo := a.Repo
		if a.Kind == "delegate" {
			parent = a.TaskID
		}
		prompt := "Paddock has already created and launched this task, its worktree, and this agent session. Perform the assigned work in this existing task. Do not create another task or session to establish the isolation requested below.\n\nAssignment:\n" + a.Text
		if signal.Source == "github" {
			if err := o.GitHub.grantSignalRecipient(signal.ID, out.TaskID); err != nil {
				return "", err
			}
			prompt += fmt.Sprintf("\nOrigin GitHub signal %s. Report results with: PADDOCK_HOME=%s %s github reply %s %s", signal.ID, quote(o.Service.Store.Home), quote(o.Service.Executable), signal.ID, quote("Describe the result and validation."))
		}
		task, err := o.Service.Run(ctx, RunOptions{ID: out.TaskID, Prompt: prompt, Parent: parent, Agent: a.Agent, Repo: repo, Base: a.Base, SignalID: signal.Key(), RelatedTaskID: signal.TaskID})
		return task.ID, err
	}
	return "", fmt.Errorf("unknown action")
}
