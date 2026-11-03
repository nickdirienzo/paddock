package paddock

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LifecycleConfig struct {
	WorktreeTTL string `json:"worktree_ttl"`
	// Only ignored files beneath these relative directories may be discarded.
	DisposableDirectories []string `json:"disposable_directories,omitempty"`
}

func (s *Store) LifecycleConfig() (LifecycleConfig, error) {
	c := LifecycleConfig{WorktreeTTL: "168h"}
	b, err := os.ReadFile(filepath.Join(s.Home, "lifecycle.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, c.validate()
}

func (c LifecycleConfig) validate() error {
	d, err := time.ParseDuration(c.WorktreeTTL)
	if err != nil || d <= 0 {
		return fmt.Errorf("worktree TTL must be a positive duration, e.g. 168h")
	}
	for _, p := range c.DisposableDirectories {
		if !filepath.IsLocal(p) || p == "." || filepath.Clean(p) != p || p == ".git" || strings.HasPrefix(p, ".git"+string(os.PathSeparator)) {
			return fmt.Errorf("invalid disposable directory %q", p)
		}
	}
	return nil
}

func (s *Store) SetLifecycleConfig(c LifecycleConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	return s.withLock(func() error { return writeJSONAtomic(filepath.Join(s.Home, "lifecycle.json"), c) })
}

func newConversationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Resolve existing ancestors too: a removed worktree can still have /var vs
// /private/var (or another symlinked parent) in its saved native/Git metadata.
func canonicalWorkspacePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path || parent == "." {
		return filepath.Clean(path)
	}
	return filepath.Join(canonicalWorkspacePath(parent), filepath.Base(path))
}

// Separate operation and process leases: the supervisor keeps the process lease
// for its entire lifetime; CLI operations serialize without blocking that lease.
func (s *Store) taskLease(id, kind string) (func(), error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid task ID")
	}
	f, err := os.OpenFile(filepath.Join(s.Home, "tasks", id+"."+kind+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("task %s has an active %s lease", id, kind)
	}
	return func() { unlock(f); f.Close() }, nil
}

func (s *Store) TouchActivity(id string) error {
	return s.update(id, func(t *Task) error { t.LastActivityAt = now(); return nil })
}

func (s Service) Resume(ctx context.Context, id string) (Task, error) {
	t, err := s.Store.Get(id)
	if err != nil {
		return t, err
	}
	release, err := s.Store.taskLease(t.ID, "operation")
	if err != nil {
		return t, err
	}
	defer release()
	return s.resumeLocked(ctx, t.ID)
}

func (s Service) resumeLocked(ctx context.Context, id string) (Task, error) {
	t, err := s.Store.Get(id)
	if err != nil {
		return t, err
	}
	t, obs, err := s.Refresh(ctx, t)
	if err != nil {
		return t, err
	}
	if obs.State == "running" {
		if t.Status != "running" {
			return t, fmt.Errorf("terminal is running but agent state is %s; inspect before resuming", t.Status)
		}
		return t, nil
	}
	if obs.State == "unknown" {
		return t, fmt.Errorf("cannot safely resume: %s", obs.Error)
	}
	if t.Status == "preparing" || t.Status == "starting" {
		return t, fmt.Errorf("task launch is pending; inspect before resuming")
	}
	release, err := s.Store.taskLease(id, "process")
	if err != nil {
		return t, err
	}
	release()
	if t.RuntimeName() == "openshell" {
		if err = s.startTaskSandbox(ctx, &t); err != nil {
			return t, err
		}
	}
	if t.Session.ConversationID == "" {
		if t.Session.Agent != "codex" {
			return t, fmt.Errorf("native conversation ID was not recorded; cannot safely resume")
		}
		thread, e := codexTaskThread(ctx, t)
		if e != nil {
			return t, fmt.Errorf("cannot recover native conversation identity: %w", e)
		}
		t.Session.ConversationID = thread
		if err = s.Store.UpdateSession(id, func(v *Session) { v.ConversationID = thread }); err != nil {
			return t, err
		}
	}
	// Validate the persisted conversation before launching. Never silently fork.
	if err = verifyConversation(ctx, t); err != nil {
		return t, err
	}
	restored, err := restoreWorktree(ctx, t)
	if err != nil {
		return t, err
	}
	if restored {
		t.WorkspaceCommit, err = taskGit(ctx, t, "rev-parse", "HEAD")
		if err != nil {
			return t, err
		}
		t.Session.ResumeNotice = "Paddock restored this worktree from Git at " + t.WorkspaceCommit + ". Build artifacts and uncommitted filesystem state were not restored. Inspect the checkout before continuing; conversation history is not a filesystem backup."
	}
	// Close only an exited terminal. An uncertain observation never permits it.
	if obs.State == "exited" {
		if err = closeExitedTerminal(ctx, t); err != nil {
			return t, err
		}
	}
	if err = s.Store.update(id, func(v *Task) error {
		v.Session.Generation++
		v.Session.Resume = true
		v.Session.ResumeNotice = t.Session.ResumeNotice
		v.Session.ID, v.Session.BlockID, v.Session.WindowID = "", "", ""
		v.Session.Name = fmt.Sprintf("paddock-%s-%d", id, v.Session.Generation)
		v.Session.ExitCode = nil
		v.Session.StartedAt, v.Session.EndedAt = "", ""
		v.Status, v.Error = "starting", ""
		v.LastActivityAt = now()
		v.WorkspaceRemovedAt = ""
		return nil
	}); err != nil {
		return t, err
	}
	t, err = s.Store.Get(id)
	if err != nil {
		return t, err
	}
	return s.launchTask(ctx, t)
}

func closeExitedTerminal(ctx context.Context, t Task) error {
	term := terminalFor(t.Session)
	session, obs := term.Inspect(ctx, t)
	if obs.State == "missing" {
		return nil
	}
	if obs.State != "exited" {
		return fmt.Errorf("refusing to close a terminal whose process is not confirmed exited")
	}
	if session.Backend == "rex" {
		// Closing only the owned block preserves any panes the user added.
		_, err := (terminal{path: session.TerminalPath, endpoint: session.Endpoint}).rex(ctx, "block", "close", "-s", session.ID, session.BlockID)
		return err
	}
	return nil // Screen removes exited sessions itself.
}

func workspaceIdentity(ctx context.Context, t Task) error {
	if t.RuntimeName() == "openshell" {
		return sandboxWorkspaceIdentity(ctx, t)
	}
	root, err := git(ctx, t.Worktree, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	want, err := filepath.EvalSymlinks(t.Worktree)
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(root)
	if err != nil || actual != want {
		return fmt.Errorf("path is not the task's worktree root")
	}
	common, err := git(ctx, t.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	a, err := filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	b, err := filepath.EvalSymlinks(t.CommonDir)
	if err != nil || a != b {
		return fmt.Errorf("worktree repository no longer matches task")
	}
	branch, err := git(ctx, t.Worktree, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || branch != "refs/heads/"+t.Branch {
		return fmt.Errorf("worktree branch no longer matches task")
	}
	return nil
}

func restoreWorktree(ctx context.Context, t Task) (bool, error) {
	if t.RuntimeName() == "openshell" {
		return restoreSandboxWorktree(ctx, t)
	}
	if _, err := os.Lstat(t.Worktree); err == nil {
		return false, workspaceIdentity(ctx, t)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	head, err := git(ctx, t.CommonDir, "rev-parse", "--verify", "refs/heads/"+t.Branch+"^{commit}")
	if err != nil {
		return false, err
	}
	if t.WorkspaceRemovedAt != "" && t.WorkspaceCommit != "" && head != t.WorkspaceCommit {
		return false, fmt.Errorf("retained branch moved since GC; refusing to guess a recovery commit")
	}
	// A manually removed directory can leave a stale registration. Allow only
	// that exact absent path to be recreated; never prune unrelated worktrees,
	// override a lock, or check out a branch that is in use somewhere else.
	listing, err := git(ctx, t.CommonDir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	stale := false
	wantPath := canonicalWorkspacePath(t.Worktree)
	for _, entry := range strings.Split(listing, "\x00\x00") {
		var path, branch string
		locked := false
		for _, field := range strings.Split(entry, "\x00") {
			if strings.HasPrefix(field, "worktree ") {
				path = strings.TrimPrefix(field, "worktree ")
			}
			if strings.HasPrefix(field, "branch ") {
				branch = strings.TrimPrefix(field, "branch ")
			}
			if field == "locked" || strings.HasPrefix(field, "locked ") {
				locked = true
			}
		}
		path = canonicalWorkspacePath(path)
		if branch == "refs/heads/"+t.Branch && path != wantPath {
			return false, fmt.Errorf("task branch is registered in another worktree")
		}
		if path == wantPath {
			if locked || branch != "refs/heads/"+t.Branch {
				return false, fmt.Errorf("missing worktree registration is locked or has a different branch")
			}
			stale = true
		}
	}
	args := []string{"-c", "core.hooksPath=" + os.DevNull, "worktree", "add"}
	if stale {
		args = append(args, "--force")
	} // Recreation only; never used for deletion.
	_, err = git(ctx, t.CommonDir, append(args, "--", t.Worktree, t.Branch)...)
	return err == nil, err
}
