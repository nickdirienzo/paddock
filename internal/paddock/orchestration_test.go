package paddock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testPlanner struct {
	calls  int
	decide func(PlanningInput) (Decision, error)
}

func (p *testPlanner) Name() string { return "test-planner" }
func (p *testPlanner) Decide(_ context.Context, in PlanningInput) (Decision, error) {
	p.calls++
	return p.decide(in)
}
func decisions(a ...Action) Decision { return Decision{Requests: []ContextRequest{}, Actions: a} }
func orchestrationFixture(t *testing.T) (Orchestrator, *testPlanner, Signal, OrchestrationOptions, string) {
	g, _, _, w, log := githubFixture(t)
	p := &testPlanner{}
	o := Orchestrator{Service: g.Service, Planner: p}
	sig := Signal{Source: "cli", ID: "test-signal", Body: "route this", TaskID: w.TaskID}
	opts := OrchestrationOptions{Policy: OrchestrationPolicy{Tasks: []string{w.TaskID}}}
	return o, p, sig, opts, log
}
func TestOrchestrationContextDryRunAndReplay(t *testing.T) {
	o, p, sig, opts, log := orchestrationFixture(t)
	p.decide = func(in PlanningInput) (Decision, error) {
		if len(in.Tasks) != 1 || in.Tasks[0].ID != sig.TaskID {
			t.Fatal("missing real task catalog")
		}
		if len(in.Context) == 0 {
			return Decision{Requests: []ContextRequest{{Kind: "task", ID: sig.TaskID}}}, nil
		}
		if in.Context[0].Error != "" {
			t.Fatal(in.Context)
		}
		return decisions(Action{Kind: "message", TaskID: sig.TaskID, Text: "Fix the race", Reason: "existing owner"}), nil
	}
	opts.DryRun = true
	r, err := o.Process(context.Background(), sig, opts)
	if err != nil || r.Status != "dry_run" || len(r.Rounds) != 2 {
		t.Fatalf("preview: %+v %v", r, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("dry run delivered work")
	}
	opts.DryRun = false
	r, err = o.Process(context.Background(), sig, opts)
	if err != nil || r.Status != "done" {
		t.Fatalf("execute: %+v %v", r, err)
	}
	calls := p.calls
	again, err := o.Process(context.Background(), sig, opts)
	if err != nil || again.Status != "done" || p.calls != calls {
		t.Fatal("replanned completed signal", err)
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), "queue --thread") != 1 {
		t.Fatal("duplicated message")
	}
	sig.Body = "different content"
	if _, err = o.Process(context.Background(), sig, opts); err == nil {
		t.Fatal("accepted conflicting signal ID")
	}
}
func TestOrchestrationValidationPreventsPartialActions(t *testing.T) {
	o, p, sig, opts, log := orchestrationFixture(t)
	p.decide = func(PlanningInput) (Decision, error) {
		return decisions(Action{Kind: "message", TaskID: sig.TaskID, Text: "valid first", Reason: "owner"}, Action{Kind: "create_task", Repo: "/unapproved", Agent: "codex", Text: "bad second", Reason: "injection"}), nil
	}
	if _, err := o.Process(context.Background(), sig, opts); err == nil {
		t.Fatal("accepted out-of-scope action")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("partially executed invalid plan")
	}
	r, err := o.Service.Store.SignalRecord(sig.Source, sig.ID)
	if err != nil || r.Error == "" {
		t.Fatal("failure not recorded")
	}
}
func TestOrchestrationCrashNeverReplays(t *testing.T) {
	o, p, sig, opts, log := orchestrationFixture(t)
	p.decide = func(PlanningInput) (Decision, error) {
		return decisions(Action{Kind: "message", TaskID: sig.TaskID, Text: "once", Reason: "owner"}), nil
	}
	opts.DryRun = true
	r, err := o.Process(context.Background(), sig, opts)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = "executing"
	r.Outcomes[0].Status = "executing"
	if err = o.saveSignal(&r); err != nil {
		t.Fatal(err)
	}
	opts.DryRun = false
	r, err = o.Process(context.Background(), sig, opts)
	if err != nil || r.Status != "uncertain" || p.calls != 1 {
		t.Fatalf("recovery: %+v %v", r, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("replayed uncertain action")
	}
}
func TestOrchestrationOfflineDestinationCanResumeWithoutReplanning(t *testing.T) {
	o, p, sig, opts, log := orchestrationFixture(t)
	p.decide = func(PlanningInput) (Decision, error) {
		return decisions(Action{Kind: "message", TaskID: sig.TaskID, Text: "later", Reason: "owner"}), nil
	}
	if err := o.Service.Store.Status(sig.TaskID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	r, err := o.Process(context.Background(), sig, opts)
	if err != nil || r.Status != "pending" {
		t.Fatalf("offline: %+v %v", r, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("offline delivery")
	}
	if err = o.Service.Store.Status(sig.TaskID, "running", ""); err != nil {
		t.Fatal(err)
	}
	r, err = o.Process(context.Background(), sig, opts)
	if err != nil || r.Status != "done" || p.calls != 1 {
		t.Fatalf("resume: %+v %v", r, err)
	}
}
func TestOrchestrationReadsCannotEscapeScope(t *testing.T) {
	o, _, sig, opts, _ := orchestrationFixture(t)
	for _, request := range []ContextRequest{{Kind: "task", ID: "unknown"}, {Kind: "repo", ID: "/"}, {Kind: "pr", ID: "private/repo#1"}, {Kind: "shell", ID: "touch /tmp/pwn"}} {
		e := o.readContext(context.Background(), sig, opts.Policy, request)
		if e.Error == "" || e.Data != nil {
			t.Fatalf("read escaped scope: %+v", e)
		}
	}
}
func TestOrchestrationPlannerFailureDoesNotConsumeSignal(t *testing.T) {
	o, p, sig, opts, _ := orchestrationFixture(t)
	p.decide = func(PlanningInput) (Decision, error) { return Decision{}, errors.New("login expired") }
	if r, err := o.Process(context.Background(), sig, opts); err == nil || r.Status != "planning_failed" || r.Error == "" {
		t.Fatal("planner failure not durably distinguished from active planning", r, err)
	}
	p.decide = func(PlanningInput) (Decision, error) {
		return decisions(Action{Kind: "ignore", Reason: "irrelevant"}), nil
	}
	r, err := o.Process(context.Background(), sig, opts)
	if err != nil || r.Status != "done" {
		t.Fatalf("retry: %+v %v", r, err)
	}
}
func TestGitHubPlannerAuthorizationCrossTaskAndReply(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	other := taskFixture()
	other.ID = newID()
	other.Status = "completed"
	if err := s.Insert(other); err != nil {
		t.Fatal(err)
	}
	// Origin offline; the useful destination is the original live fixture task.
	origin := w
	origin.TaskID = other.ID
	origin.Routing = "planner"
	origin.Orchestration = OrchestrationPolicy{Tasks: []string{other.ID, w.TaskID}}
	p := &testPlanner{decide: func(in PlanningInput) (Decision, error) {
		return decisions(Action{Kind: "message", TaskID: w.TaskID, Text: "handle cross task request", Reason: "live specialist"}), nil
	}}
	g.Planner = p
	if err := s.UpdateGitHub(func(v *GitHubState) error { v.Watches[w.Key()] = origin; return nil }); err != nil {
		t.Fatal(err)
	}
	remote.comments = []GitHubComment{testComment(1, "actionable", 7), testComment(2, "untrusted", 99)}
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 2 || p.calls != 1 {
		t.Fatalf("poll: %+v %v calls=%d", events, err, p.calls)
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), "queue --thread") != 1 {
		t.Fatal("did not route cross-task")
	}
	t.Setenv("PADDOCK_TASK_ID", w.TaskID)
	if _, err := g.EnqueueReply(events[0].ID, "handled by specialist"); err != nil {
		t.Fatal("recipient cannot reply", err)
	}
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.replies) != 1 || p.calls != 1 {
		t.Fatal("repeat planned or reply not published")
	}
	// Revoking the original author prevents further execution, even with a saved decision.
	if err := s.UpdateGitHub(func(v *GitHubState) error {
		x := v.Watches[w.Key()]
		x.Authors = map[string]int64{}
		v.Watches[w.Key()] = x
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.EnqueueReply(events[0].ID, "after revocation"); err == nil {
		t.Fatal("revoked author still grants reply authority")
	}
}
func TestCodexPlannerUsesSubscriptionAndNoExecutionTools(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	env := filepath.Join(dir, "env")
	fake := script(t, `printf '%s\n' "$@" > `+quote(args)+`
env > `+quote(env)+`
while test "$#" -gt 0; do
 if test "$1" = '--output-last-message'; then shift; result="$1"; fi
 shift
done
cat >/dev/null
printf '%s' '{"requests":[],"actions":[{"kind":"ignore","task_id":"","repo":"","base":"","agent":"","text":"","reason":"irrelevant"}]}' > "$result"
`)
	t.Setenv("PADDOCK_CODEX_BIN", fake)
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	t.Setenv("GH_TOKEN", "must-not-leak")
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", "must-not-leak")
	_, err := (CodexPlanner{Model: "chosen-model", Instructions: "prefer reuse"}).Decide(context.Background(), PlanningInput{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(args)
	for _, flag := range []string{"--ephemeral", "--ignore-user-config", "read-only", "forced_login_method=\"chatgpt\"", "shell_tool", "hooks", "plugins", "apps", "chosen-model", "--output-schema"} {
		if !strings.Contains(string(data), flag) {
			t.Fatal("missing planner protection", flag)
		}
	}
	data, _ = os.ReadFile(env)
	if strings.Contains(string(data), "must-not-leak") {
		t.Fatal("credentials leaked into planner")
	}
}
func TestPlannerConfigurationRoundTrip(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	config := PlannerConfig{Model: "my-model", Instructions: "Prefer specialists."}
	if err := s.SetPlannerConfig(config); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.PlannerConfig()
	if err != nil || loaded != config {
		t.Fatalf("config: %+v %v", loaded, err)
	}
	info, err := os.Stat(filepath.Join(s.Home, "planner.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("configuration is not private")
	}
}

func TestOrchestrationDuplicateTargetAndChangedScope(t *testing.T) {
	o, p, sig, opts, log := orchestrationFixture(t)
	p.decide = func(PlanningInput) (Decision, error) {
		return decisions(
			Action{Kind: "message", TaskID: sig.TaskID, Text: "first", Reason: "one"},
			Action{Kind: "message", TaskID: sig.TaskID, Text: "second", Reason: "two"}), nil
	}
	if _, err := o.Process(context.Background(), sig, opts); err == nil {
		t.Fatal("duplicated destination accepted")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("duplicate-target plan was partially executed")
	}
	p.decide = func(PlanningInput) (Decision, error) {
		return decisions(Action{Kind: "message", TaskID: sig.TaskID, Text: "later", Reason: "owner"}), nil
	}
	if err := o.Service.Store.Status(sig.TaskID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Process(context.Background(), sig, opts); err != nil {
		t.Fatal(err)
	}
	opts.Policy.Tasks = nil
	if _, err := o.Process(context.Background(), sig, opts); err == nil {
		t.Fatal("pending action bypassed revoked scope")
	}
}

func TestGitHubPlannerIgnoreAndReplyWithoutAgentWork(t *testing.T) {
	for _, kind := range []string{"ignore", "reply"} {
		t.Run(kind, func(t *testing.T) {
			g, remote, s, w, log := githubFixture(t)
			w.Routing = "planner"
			w.Orchestration = OrchestrationPolicy{Tasks: []string{w.TaskID}}
			if err := s.UpdateGitHub(func(v *GitHubState) error { v.Watches[w.Key()] = w; return nil }); err != nil {
				t.Fatal(err)
			}
			text := ""
			if kind == "reply" {
				text = "The task is available."
			}
			p := &testPlanner{decide: func(PlanningInput) (Decision, error) {
				return decisions(Action{Kind: kind, Text: text, Reason: "discussion only"}), nil
			}}
			g.Planner = p
			remote.comments = []GitHubComment{testComment(1, "discussion", 7)}
			events, err := g.Poll(context.Background())
			if err != nil || len(events) != 1 || events[0].Plan.Action != kind || events[0].Status != "done" {
				t.Fatalf("%+v %v", events, err)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("unneeded agent work")
			}
			if remote.reactionCalls != 0 {
				t.Fatal("reaction falsely claimed message delivery")
			}
			if _, err := g.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			expected := 0
			if kind == "reply" {
				expected = 1
			}
			if len(remote.replies) != expected || p.calls != 1 {
				t.Fatal("duplicate planning or replies")
			}
		})
	}
}

func TestOrchestrationConcurrentSignalLease(t *testing.T) {
	o, p, sig, opts, log := orchestrationFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	p.decide = func(PlanningInput) (Decision, error) {
		close(entered)
		<-release
		return decisions(Action{Kind: "message", TaskID: sig.TaskID, Text: "once", Reason: "owner"}), nil
	}
	go func() { _, err := o.Process(context.Background(), sig, opts); done <- err }()
	<-entered
	_, err := o.Process(context.Background(), sig, opts)
	close(release)
	if err == nil || !strings.Contains(err.Error(), "already being processed") {
		t.Fatal("concurrent signal accepted", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), "queue --thread") != 1 {
		t.Fatal("concurrent duplicate delivery")
	}
}

func TestPerceptionDoesNotMutateTaskAndReadsPRContext(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	task, _ := s.Get(w.TaskID)
	if err := os.WriteFile(task.Session.TerminalPath, []byte("#!/bin/sh\necho 'No Sockets found in /tmp/screen.'; exit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	o := Orchestrator{Service: g.Service, GitHub: &g}
	p := OrchestrationPolicy{Tasks: []string{w.TaskID}}
	input, err := o.perceive(context.Background(), Signal{}, p)
	if err != nil || input.Tasks[0].SessionState != "offline" {
		t.Fatal(input, err)
	}
	after, _ := s.Get(w.TaskID)
	if after.Status != task.Status || after.UpdatedAt != task.UpdatedAt {
		t.Fatal("perception mutated task")
	}
	remote.pr.Title = "Health endpoint"
	remote.pr.Body = "Relevant implementation discussion"
	remote.comments = []GitHubComment{testComment(1, "Please document status ok", 7)}
	e := o.readContext(context.Background(), Signal{}, p, ContextRequest{Kind: "pr", ID: w.Key()})
	if e.Error != "" {
		t.Fatal(e.Error)
	}
	data := e.Data.(map[string]any)
	if data["pull_request"].(PullRequest).Body != remote.pr.Body || len(data["recent_comments"].([]GitHubComment)) != 1 {
		t.Fatal("missing PR context", data)
	}
}
