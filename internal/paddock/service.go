package paddock

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Service struct {
	Store      *Store
	Executable string
	// Tests can supply deterministic native-delivery behavior for routing fixtures.
	// Production services leave this nil and use the OpenShell-only Message path.
	messageDelivery func(context.Context, Task, string) (string, error)
}

func (s Service) sendMessage(ctx context.Context, task Task, body string) (string, error) {
	if s.messageDelivery != nil {
		return s.messageDelivery(ctx, task, body)
	}
	return s.Message(ctx, task, body)
}

type RunOptions struct {
	Prompt, Parent, Agent, Backend, Repo, Base, ID            string
	SignalID, RelatedTaskID, Runtime, Role                    string
	Identity, Profile, Requester, RequesterSource, ExternalID string
	Context                                                   map[string]string
	provenance                                                TaskProvenance
}

func ValidateStateHome(home, repo string) error {
	if rel, err := filepath.Rel(repo, home); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("PADDOCK_HOME must be outside the repository checkout")
	}
	return nil
}

func (s Service) Run(ctx context.Context, o RunOptions) (Task, error) {
	var empty Task
	if o.Parent != "" {
		parent, err := s.Store.Get(o.Parent)
		if err != nil {
			return empty, err
		}
		caller, err := LocalCaller()
		if err != nil {
			return empty, err
		}
		if parent.Provenance.Caller != "" && parent.Provenance.Caller != caller {
			return empty, fmt.Errorf("caller is not the owner of parent task %s", parent.ID)
		}
		o.Parent = parent.ID
		if o.Identity == "" {
			o.Identity = parent.Provenance.Identity
		}
		if o.Profile == "" {
			o.Profile = parent.Provenance.Profile
		}
		if parent.Provenance.Profile == "investigation" && normalizedProfile(o.Profile) != "investigation" {
			return empty, fmt.Errorf("child task cannot exceed its parent's investigation profile")
		}
	}
	provenance, err := s.resolveProvenance(o)
	if err != nil {
		return empty, err
	}
	o.provenance = provenance
	if provenance.ExternalID != "" {
		prior, found, err := s.Store.FindExternal(provenance.Caller, provenance.RequesterSource, provenance.ExternalID)
		if err != nil {
			return empty, err
		}
		if found {
			if prior.RuntimeName() != "openshell" {
				return prior, fmt.Errorf("task %s used the host runtime; agent execution requires OpenShell", prior.ID)
			}
			if o.Repo != "" {
				requested, resolveErr := ResolveRepoArgument(s.Store.Home, o.Repo)
				if resolveErr != nil {
					return prior, resolveErr
				}
				repository, discoverErr := discoverRepository(ctx, requested)
				if discoverErr != nil {
					return prior, discoverErr
				}
				if repository.Root != prior.Repo {
					return prior, fmt.Errorf("external ID already belongs to task %s in a different repository", prior.ID)
				}
			}
			requestedAgent := o.Agent
			if requestedAgent == "" {
				requestedAgent = "codex"
			}
			if prior.Prompt != o.Prompt || (o.Identity != "" && prior.Provenance.Identity != o.Identity) || prior.Provenance.Profile != provenance.Profile || prior.Provenance.Requester != o.Requester || !maps.Equal(prior.Provenance.Context, o.Context) || prior.Session.Agent != requestedAgent ||
				(o.Base != "" && o.Base != prior.Base) || (o.Parent != "" && o.Parent != prior.ParentID) || (o.Runtime != "" && o.Runtime != prior.RuntimeName()) || (o.Backend != "" && o.Backend != "auto" && o.Backend != prior.Session.Backend) || (o.Role != "" && o.Role != prior.Role) || (o.SignalID != "" && o.SignalID != prior.SignalID) || (o.RelatedTaskID != "" && o.RelatedTaskID != prior.RelatedTaskID) {
				return prior, fmt.Errorf("external ID already belongs to task %s with different dispatch data", prior.ID)
			}
			return prior, nil
		}
	}
	if o.Runtime == "" {
		var err error
		o.Runtime, err = s.Store.DefaultRuntime()
		if err != nil {
			return empty, err
		}
	}
	if o.Runtime != "openshell" {
		return empty, fmt.Errorf("agent execution requires OpenShell; host runtime is unavailable")
	}
	if o.Role != "" {
		return empty, fmt.Errorf("Team Principal requires a supported OpenShell execution path")
	}
	if o.Role != "" && (o.Role != "tp" || o.Runtime != "host") {
		return empty, fmt.Errorf("Team Principal requires the host runtime")
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
	return s.runOpenShell(ctx, o, repo)
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
	if t.Status == "preparing" && time.Since(age) > 2*time.Minute {
		active, err := s.Store.preparationActive(t.ID)
		if err != nil {
			return t, obs, err
		}
		if !active {
			if err := s.Store.update(t.ID, func(current *Task) error {
				if current.Status == "preparing" {
					current.Status, current.Error, current.LastActivityAt = "interrupted", "task preparation ended without a recorded launch", now()
				}
				return nil
			}); err != nil {
				return t, obs, err
			}
		}
	}
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
