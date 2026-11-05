package paddock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Service struct {
	Store      *Store
	Executable string
}
type RunOptions struct{ Prompt, Parent, Agent, Backend, Repo, Base, ID, SignalID, RelatedTaskID, Runtime string }

func ValidateStateHome(home, repo string) error {
	if rel, err := filepath.Rel(repo, home); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("PADDOCK_HOME must be outside the repository checkout")
	}
	return nil
}

func (s Service) Run(ctx context.Context, o RunOptions) (Task, error) {
	var empty Task
	if o.Runtime == "" {
		var err error
		o.Runtime, err = s.Store.DefaultRuntime()
		if err != nil {
			return empty, err
		}
	}
	if o.Runtime != "host" && o.Runtime != "openshell" {
		return empty, fmt.Errorf("unknown runtime %q (use openshell or host)", o.Runtime)
	}
	if strings.TrimSpace(o.Prompt) == "" {
		return empty, fmt.Errorf("a non-empty task prompt is required")
	}
	if o.Agent == "" {
		o.Agent = "codex"
	}
	if o.Agent != "codex" && o.Agent != "claude" {
		return empty, fmt.Errorf("unknown agent %q (use codex or claude)", o.Agent)
	}
	if o.Parent != "" {
		parent, err := s.Store.Get(o.Parent)
		if err != nil {
			return empty, err
		}
		o.Parent = parent.ID
		if o.Repo == "" {
			o.Repo = parent.Repo
		}
		if o.Backend == "" {
			o.Backend = parent.Session.Backend
		}
	}
	if o.Repo == "" {
		var err error
		o.Repo, err = os.Getwd()
		if err != nil {
			return empty, err
		}
	}
	resolved, err := ResolveRepoArgument(s.Store.Home, o.Repo)
	if err != nil {
		return empty, err
	}
	o.Repo = resolved
	repo, err := ResolveRepository(ctx, o.Repo, o.Base)
	if err != nil {
		return empty, err
	}
	if o.Runtime == "openshell" {
		return s.runOpenShell(ctx, o, repo)
	}
	trusted, err := RepoTrusted(s.Store.Home, repo.CommonDir)
	if err != nil {
		return empty, err
	}
	agentPath, err := executable("PADDOCK_"+strings.ToUpper(o.Agent)+"_BIN", o.Agent)
	if err != nil {
		return empty, fmt.Errorf("%s is not installed or executable: %w", o.Agent, err)
	}
	_, session, err := SelectTerminal(ctx, o.Backend)
	if err != nil {
		return empty, err
	}
	id := o.ID
	if id == "" {
		id = newID()
	}
	if !validID(id) {
		return empty, fmt.Errorf("invalid reserved task ID")
	}
	session.Agent = o.Agent
	session.Generation = 1
	if o.Agent == "codex" {
		session.AgentHome = os.Getenv("CODEX_HOME")
		if session.AgentHome == "" {
			if h, e := os.UserHomeDir(); e == nil {
				session.AgentHome = filepath.Join(h, ".codex")
			}
		}
	}
	if o.Agent == "claude" {
		session.ConversationID = newConversationID()
		session.AgentHome = os.Getenv("CLAUDE_CONFIG_DIR")
		if session.AgentHome == "" {
			if h, e := os.UserHomeDir(); e == nil {
				session.AgentHome = filepath.Join(h, ".claude")
			}
		}
	}
	if session.AgentHome != "" {
		session.AgentHome, err = filepath.Abs(session.AgentHome)
		if err != nil {
			return empty, err
		}
	}
	session.TrustProject = trusted && o.Agent == "codex"
	session.AgentPath = agentPath
	session.Name = "paddock-" + id
	t := Task{ID: id, SignalID: o.SignalID, RelatedTaskID: o.RelatedTaskID, ParentID: o.Parent, Prompt: o.Prompt, Repo: repo.Root, CommonDir: repo.CommonDir, Base: repo.Base, BaseCommit: repo.Commit,
		Branch: "paddock/" + id, Worktree: filepath.Join(s.Store.Home, "worktrees", id), Status: "preparing", CreatedAt: now(), UpdatedAt: now(), LastActivityAt: now(), Session: session}
	// Never put state or new worktrees inside the caller's checkout.
	t.Runtime = "host"
	if err = ValidateStateHome(s.Store.Home, repo.Root); err != nil {
		return empty, err
	}
	if err = s.Store.Insert(t); err != nil {
		return empty, err
	}
	fail := func(cause error) (Task, error) {
		if e := s.Store.Status(id, "failed", cause.Error(), "preparing", "starting"); e != nil {
			return t, fmt.Errorf("task %s: %v; recording failure: %w", id, cause, e)
		}
		current, _ := s.Store.Get(id)
		return current, fmt.Errorf("task %s: %w (inspect with paddock show %s)", id, cause, id)
	}
	if err = CreateWorktree(ctx, t); err != nil {
		return fail(err)
	}
	if err = s.Store.Status(id, "starting", ""); err != nil {
		return t, err
	}
	t.Status = "starting"
	return s.launchTask(ctx, t)
}

func (s Service) launchTask(ctx context.Context, t Task) (Task, error) {
	term := terminalFor(t.Session)
	var err error
	fail := func(cause error) (Task, error) {
		if err := s.Store.Status(t.ID, "failed", cause.Error(), "starting"); err != nil {
			return t, err
		}
		current, _ := s.Store.Get(t.ID)
		return current, fmt.Errorf("task %s: %w", t.ID, cause)
	}
	if err = s.prepareRexGroup(ctx, &t); err != nil {
		return fail(err)
	}
	launched, launchErr := term.Launch(ctx, t, s.Executable, s.Store.Home)
	if launchErr != nil {
		// A timed-out launch may have succeeded. Inspect the deterministic name;
		// never create a second session as an automatic retry.
		recovered, obs := term.Inspect(ctx, t)
		if obs.State == "running" || obs.State == "exited" {
			launched = recovered
		} else {
			if obs.State == "unknown" {
				return t, fmt.Errorf("task %s launch outcome is uncertain: %w; use show to reconcile before retrying", t.ID, launchErr)
			}
			current, e := s.Store.Get(t.ID)
			if e == nil && current.Status != "starting" {
				return current, nil
			}
			return fail(launchErr)
		}
	}
	if err = s.Store.UpdateSession(t.ID, func(v *Session) { v.ID = launched.ID; v.BlockID = launched.BlockID; v.WindowID = launched.WindowID }); err != nil {
		return t, err
	}
	// Wait briefly for the supervisor to claim the task, catching silent failures.
	for i := 0; i < 50; i++ {
		t, err = s.Store.Get(t.ID)
		if err != nil {
			return t, err
		}
		if t.Status != "starting" {
			return t, nil
		}
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	_, obs := term.Inspect(ctx, t)
	if obs.State == "missing" || obs.State == "exited" {
		return fail(fmt.Errorf("terminal exited before the task supervisor started"))
	}
	return t, nil
}

func (s Service) Refresh(ctx context.Context, t Task) (Task, Observation, error) {
	session, obs := terminalFor(t.Session).Inspect(ctx, t)
	if session.ID != t.Session.ID || session.BlockID != t.Session.BlockID || session.WindowID != t.Session.WindowID {
		if err := s.Store.update(t.ID, func(current *Task) error {
			if current.Session.Generation == t.Session.Generation {
				current.Session.ID, current.Session.BlockID, current.Session.WindowID = session.ID, session.BlockID, session.WindowID
			}
			return nil
		}); err != nil {
			return t, obs, err
		}
	}
	// starting has a grace period: another CLI may inspect between intent and launch.
	age, _ := time.Parse(time.RFC3339Nano, t.UpdatedAt)
	if (t.Status == "running" || (t.Status == "starting" && time.Since(age) > 30*time.Second)) && (obs.State == "missing" || obs.State == "exited") {
		if err := s.Store.update(t.ID, func(current *Task) error {
			if current.Session.Generation == t.Session.Generation && current.Status == t.Status {
				current.Status, current.Error = "interrupted", "terminal ended without a recorded agent exit"
				// A crash has no supervisor Finish event. Start retention when its
				// stopped process is first confirmed, rather than its old launch.
				current.LastActivityAt = now()
			}
			return nil
		}); err != nil {
			return t, obs, err
		}
	}
	current, err := s.Store.Get(t.ID)
	return current, obs, err
}

func (s Service) Attach(ctx context.Context, t Task, client string) error {
	return terminalFor(t.Session).Attach(ctx, t, client)
}
