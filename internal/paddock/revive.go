package paddock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Revive starts a NEW agent conversation for the same Paddock task. Only public
// archived chat context survives; source is freshly fetched origin/main. Native
// resume remains a separate operation and never silently forks a conversation.
func (s Service) Revive(ctx context.Context, id string) (Task, error) {
	t, err := s.Store.Get(id)
	if err != nil {
		return t, err
	}
	if os.Getenv("PADDOCK_TASK_ID") != "" {
		return t, fmt.Errorf("revival requires the host registry operator")
	}
	release, err := s.Store.taskLease(t.ID, "operation")
	if err != nil {
		return t, err
	}
	defer release()
	t, err = s.Store.Get(t.ID)
	if err != nil {
		return t, err
	}
	if t.Archive == nil || t.Archive.Phase != "archived" {
		return t, fmt.Errorf("task must have a confirmed expired workspace and verified chat checkpoint; partial/uncertain lifecycle operations require inspection")
	}
	if t.Role == "tp" || t.ReadOnly() {
		return t, fmt.Errorf("this task does not support fresh-workspace revival")
	}
	if t.Status == "running" || t.Status == "starting" || t.Status == "preparing" {
		return t, fmt.Errorf("task may still be running")
	}
	process, err := s.Store.taskLease(t.ID, "process")
	if err != nil {
		return t, err
	}
	process()
	_, obs := terminalFor(t.Session).Inspect(ctx, t)
	if obs.State != "missing" && obs.State != "exited" {
		return t, fmt.Errorf("terminal process is active or unknown")
	}
	if err = s.noPendingWork(t.ID); err != nil {
		return t, err
	}
	contextText, err := s.loadArchivedContext(t)
	if err != nil {
		return t, err
	}
	source, err := resolveFreshMain(ctx, t)
	if err != nil {
		return t, err
	}
	return s.revivePrepared(ctx, t, source, contextText)
}

// Called under operation lease after all read-only preflight. Kept separate for
// disposable fixtures; the public API always fetches current origin/main.
func (s Service) revivePrepared(ctx context.Context, t Task, source Repository, contextText string) (Task, error) {
	t, err := s.prepareRevivedWorkspace(ctx, t, source, contextText)
	if err != nil {
		return t, err
	}
	if t.RuntimeName() == "openshell" {
		if err = s.ensureRuntimePoller(ctx); err != nil {
			_ = s.Store.Status(t.ID, "failed", err.Error(), "starting")
			return t, err
		}
	}
	return s.launchTask(ctx, t)
}

func (s Service) prepareRevivedWorkspace(ctx context.Context, t Task, source Repository, contextText string) (Task, error) {
	next := t.Session.Generation + 1
	contextPath, err := s.writeRevivalContext(t.ID, next, contextText)
	if err != nil {
		return t, err
	}
	if t.RuntimeName() == "host" {
		if t.Worktree != filepath.Join(s.Store.Home, "worktrees", t.ID) {
			return t, fmt.Errorf("worktree is outside managed task path")
		}
		if _, err = os.Lstat(t.Worktree); !os.IsNotExist(err) {
			return t, fmt.Errorf("old workspace must be absent before fresh revival")
		}
	}
	if t.RuntimeName() == "openshell" {
		if t.OpenShell == nil {
			return t, fmt.Errorf("missing pinned sandbox configuration")
		}
		if err = confirmSandboxAbsent(ctx, t); err != nil {
			return t, err
		}
	}
	oldConversation := t.Archive.SourceConversationID
	t.Repo, t.CommonDir, t.Base, t.BaseCommit = source.Root, source.CommonDir, "origin/main", source.Commit
	t.Branch = fmt.Sprintf("paddock/%s-r%d", t.ID, next)
	t.Status, t.Error = "preparing", ""
	t.WorkspaceCommit, t.WorkspaceRemovedAt = "", ""
	t.ShippedAt, t.ShippedCommit = "", ""
	t.LastActivityAt = now()
	t.LastWorkAt = t.LastActivityAt
	t.ShipmentSource = ""
	t.PullRequests = nil
	t.Idle = nil
	t.Session.Generation = next
	t.Session.Resume = false
	t.Session.ResumeNotice = ""
	t.Session.GoalInitialized = false
	t.Session.LastTurn = nil
	t.Session.ConversationID = ""
	if t.Session.Agent == "claude" {
		t.Session.ConversationID = newConversationID()
	}
	t.Session.ContextFile = contextPath
	t.Session.ContextSHA256 = digest([]byte(contextText))
	t.Session.RevivedFrom = oldConversation
	if t.Session.Backend == "rex" && t.Session.ID != "" {
		t.Session.GroupID = t.Session.ID
	}
	t.Session.ID, t.Session.BlockID, t.Session.WindowID = "", "", ""
	t.Session.Name = fmt.Sprintf("paddock-%s-%d", t.ID, next)
	t.Session.StartedAt, t.Session.EndedAt = "", ""
	t.Session.ExitCode = nil
	checkpoint := *t.Archive
	checkpoint.Phase = "reviving"
	t.Archive = &checkpoint
	if t.RuntimeName() == "openshell" {
		sandbox := *t.OpenShell
		t.OpenShell = &sandbox
		t.OpenShell.Name = fmt.Sprintf("pd-%s-r%d", t.ID, next)
		t.OpenShell.ID = ""
		t.OpenShell.Ready = false
		// The previous task-scoped provider remains pinned. Refresh it normally;
		// provisioning must not create or import any host model credentials.
		t.Session.ContextFile = "/home/sandbox/.paddock/revival-context.md"
	}
	if err = s.Store.update(t.ID, func(v *Task) error { *v = t; return nil }); err != nil {
		return t, err
	}
	fail := func(cause error) (Task, error) {
		_ = s.Store.Status(t.ID, "failed", cause.Error(), "preparing", "starting")
		current, _ := s.Store.Get(t.ID)
		return current, fmt.Errorf("revival incomplete; chat checkpoint retained: %w; inspect task before retrying", cause)
	}
	if t.RuntimeName() == "openshell" {
		if err = s.provisionSandbox(ctx, &t); err != nil {
			return fail(err)
		}
		if err = s.installRevivalContext(ctx, t, contextPath); err != nil {
			return fail(err)
		}
	} else if err = CreateWorktree(ctx, t); err != nil {
		return fail(err)
	}
	if err = s.verifyRevivalContext(ctx, t); err != nil {
		return fail(err)
	}
	t.Archive.Phase = "revived"
	t.Status = "starting"
	if err = s.Store.update(t.ID, func(v *Task) error {
		v.OpenShell = t.OpenShell
		v.Archive = t.Archive
		v.Status = "starting"
		return nil
	}); err != nil {
		return fail(err)
	}
	return t, nil
}

func (s Service) writeRevivalContext(id string, generation int, contextText string) (string, error) {
	if !validID(id) || len(contextText) == 0 || len(contextText) > maxConversationContextBytes {
		return "", fmt.Errorf("invalid revival context")
	}
	dir := filepath.Join(s.Store.Home, "contexts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("context directory must be private and cannot be a symlink")
	}
	if err = syncDirectory(s.Store.Home); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d-%s.md", id, generation, digest([]byte(contextText))))
	if old, e := readArchiveRegular(path); e == nil {
		if string(old) != contextText {
			return "", fmt.Errorf("immutable context mismatch")
		}
		return path, privateArchivePath(path)
	} else if !os.IsNotExist(e) {
		return "", e
	}
	f, err := os.CreateTemp(dir, ".context-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(contextText); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = replaceFile(f.Name(), path); err != nil {
		return "", err
	}
	if err = syncDirectory(dir); err != nil {
		return "", err
	}
	return path, nil
}

func (s Service) installRevivalContext(ctx context.Context, t Task, local string) error {
	if _, err := t.OpenShell.Config.call(ctx, "sandbox", "upload", t.OpenShell.Name, local, t.Session.ContextFile); err != nil {
		return err
	}
	_, err := sandboxOutput(ctx, t, "python3", "-c", `import os,stat,sys
p=sys.argv[1]
assert stat.S_ISREG(os.lstat(p).st_mode)
os.chmod(p,0o600)`, t.Session.ContextFile)
	return err
}

func (s Service) verifyRevivalContext(ctx context.Context, t Task) error {
	if t.Session.ContextFile == "" {
		return nil
	}
	if t.RuntimeName() == "openshell" {
		got, err := sandboxOutput(ctx, t, "python3", "-c", `import hashlib,os,stat,sys
p=sys.argv[1]
assert stat.S_ISREG(os.lstat(p).st_mode) and os.stat(p).st_mode&0o077==0
print(hashlib.sha256(open(p,'rb').read()).hexdigest())`, t.Session.ContextFile)
		if err != nil {
			return err
		}
		if strings.TrimSpace(got) != t.Session.ContextSHA256 {
			return fmt.Errorf("revival context integrity failure")
		}
		return nil
	}
	if err := privateArchivePath(t.Session.ContextFile); err != nil {
		return err
	}
	b, err := readArchiveRegular(t.Session.ContextFile)
	if err != nil {
		return err
	}
	if digest(b) != t.Session.ContextSHA256 {
		return fmt.Errorf("revival context integrity failure")
	}
	return nil
}
