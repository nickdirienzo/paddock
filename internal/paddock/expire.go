package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const shippedWorkspaceRetention = 48 * time.Hour

// ExpireTask is an explicit operation; the existing GC policy is unchanged.
// It archives readable context for a new conversation, not a native-session backup.
func (s Service) ExpireTask(ctx context.Context, id string, apply bool) (GCResult, error) {
	if os.Getenv("PADDOCK_TASK_ID") != "" {
		return GCResult{}, fmt.Errorf("workspace expiry requires the host registry operator")
	}
	processes := workspaceProcesses
	t, err := s.Store.Get(id)
	if err != nil {
		return GCResult{}, err
	}
	if t.RuntimeName() == "openshell" {
		processes = sandboxExpiryProcesses
	}
	return s.expireTask(ctx, id, apply, gcChecks{processes, s.noOpenPulls})
}

func (s Service) expireTask(ctx context.Context, id string, apply bool, checks gcChecks) (r GCResult, err error) {
	t, err := s.Store.Get(id)
	if err != nil {
		return r, err
	}
	r = GCResult{TaskID: t.ID, Worktree: t.Worktree}
	fail := func(e error) (GCResult, error) { r.Eligible = false; r.Reason = e.Error(); return r, nil }
	operation, e := s.Store.taskLease(t.ID, "operation")
	if e != nil {
		return fail(e)
	}
	defer operation()
	t, e = s.Store.Get(t.ID)
	if e != nil {
		return r, e
	}
	if t.ReadOnly() {
		return fail(fmt.Errorf("investigation workspace expiry is not qualified"))
	}
	if t.Role == "tp" {
		return fail(fmt.Errorf("Team Principal workspace contains durable project context"))
	}
	process, e := s.Store.taskLease(t.ID, "process")
	if e != nil {
		return fail(e)
	}
	defer process()
	if t.Status == "running" || t.Status == "starting" || t.Status == "preparing" {
		return fail(fmt.Errorf("task may still be running"))
	}
	_, obs := terminalFor(t.Session).Inspect(ctx, t)
	if obs.State != "missing" && obs.State != "exited" {
		return fail(fmt.Errorf("terminal process is active or unknown"))
	}
	if t.Archive != nil && t.Archive.Phase == "deleting" {
		if !apply {
			return fail(fmt.Errorf("deletion is uncertain; apply can verify absence without retrying deletion"))
		}
		if _, e = s.loadArchivedContext(t); e != nil {
			return fail(e)
		}
		if t.Worktree != t.Archive.PreviousWorktree || t.Branch != t.Archive.PreviousBranch {
			return fail(fmt.Errorf("uncertain deletion identity changed"))
		}
		if t.RuntimeName() == "openshell" {
			if t.OpenShell == nil || t.OpenShell.ID == "" {
				return fail(fmt.Errorf("missing original sandbox identity"))
			}
			if e = confirmSandboxAbsent(ctx, t); e != nil {
				return fail(e)
			}
		} else {
			if t.RuntimeName() != "host" || t.Worktree != filepath.Join(s.Store.Home, "worktrees", t.ID) {
				return fail(fmt.Errorf("uncertain workspace is outside managed host path"))
			}
			if _, e = os.Lstat(t.Worktree); !os.IsNotExist(e) {
				return fail(fmt.Errorf("workspace deletion remains uncertain"))
			}
		}
		checkpoint := *t.Archive
		checkpoint.Phase = "archived"
		if e = s.Store.update(t.ID, func(v *Task) error {
			v.Archive = &checkpoint
			v.WorkspaceCommit = t.ShippedCommit
			v.WorkspaceRemovedAt = now()
			return nil
		}); e != nil {
			return r, e
		}
		r.Eligible = true
		r.Removed = true
		r.Reason = "confirmed prior deletion; retained archived context"
		return r, nil
	}
	if t.Archive != nil && t.Archive.Phase != "revived" {
		return fail(fmt.Errorf("task already has an archive checkpoint; inspect its lifecycle state"))
	}
	for _, stamp := range []string{t.ShippedAt, t.LastActivityAt} {
		parsed, e := time.Parse(time.RFC3339Nano, stamp)
		if e != nil {
			return fail(fmt.Errorf("shipment and activity timestamps are required"))
		}
		if time.Since(parsed) < shippedWorkspaceRetention {
			return fail(fmt.Errorf("within 48-hour post-shipment retention period"))
		}
	}
	config, e := s.Store.LifecycleConfig()
	if e != nil {
		return r, e
	}
	if !filepath.IsAbs(t.Session.AgentHome) {
		return fail(fmt.Errorf("native agent home must be an absolute path"))
	}
	sandbox := t.RuntimeName() == "openshell"
	if sandbox {
		if t.OpenShell == nil {
			return fail(fmt.Errorf("missing sandbox identity"))
		}
		expectedHome := ""
		switch t.Session.Agent {
		case "codex":
			expectedHome = "/home/sandbox/.codex"
		case "claude":
			expectedHome = "/home/sandbox/.claude"
		}
		if expectedHome == "" || t.Session.AgentHome != expectedHome {
			return fail(fmt.Errorf("sandbox native agent home does not match the supported agent"))
		}
	}
	started := false
	deleted := false
	if sandbox {
		state, e := inspectTaskSandbox(ctx, t)
		if e != nil {
			return fail(e)
		}
		if !t.OpenShell.Ready || t.OpenShell.ID == "" || state.Phase != "Stopped" {
			return fail(fmt.Errorf("sandbox must be identified and confirmed stopped before expiry"))
		}
		if t.ReadOnly() {
			return fail(fmt.Errorf("investigation sandbox expiry is not qualified"))
		}
		if _, e = t.OpenShell.Config.call(ctx, "sandbox", "start", t.OpenShell.Name); e != nil {
			return fail(e)
		}
		started = true
		defer func() {
			if started && !deleted {
				stopCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				if _, stopErr := t.OpenShell.Config.call(stopCtx, "sandbox", "stop", t.OpenShell.Name); stopErr != nil {
					r.Reason += "; maintenance stop failed: " + stopErr.Error()
				}
			}
		}()
		if t.OpenShell.Provider != "" {
			if e = s.updateSandboxCredential(ctx, &t, false); e != nil {
				return fail(e)
			}
		}
	} else {
		if t.RuntimeName() != "host" || t.Worktree != filepath.Join(s.Store.Home, "worktrees", t.ID) {
			return fail(fmt.Errorf("workspace is outside the managed host task path"))
		}
		info, e := os.Lstat(t.Worktree)
		if e != nil {
			return fail(e)
		}
		if !info.IsDir() {
			return fail(fmt.Errorf("worktree is not a directory or is a symlink"))
		}
	}
	check := func() error {
		if e := shipmentGate(ctx, t); e != nil {
			return e
		}
		var e error
		if sandbox {
			e = sandboxWorkspaceIdentity(ctx, t)
		} else {
			e = workspaceIdentity(ctx, t)
		}
		if e != nil {
			return e
		}
		if e = cleanWorkspace(ctx, t, config); e != nil {
			return e
		}
		if e = verifyConversation(ctx, t); e != nil {
			return fmt.Errorf("conversation is unavailable: %w", e)
		}
		if e = s.noPendingWork(t.ID); e != nil {
			return e
		}
		if sandbox {
			if _, e = sandboxOutput(ctx, t, "python3", "-c", sandboxExpiryScratchScript); e != nil {
				return e
			}
		}
		if e = checks.processes(ctx, t); e != nil {
			return e
		}
		if e = pushedWorkspace(ctx, t); e != nil {
			return e
		}
		if e = checks.pulls(ctx, t); e != nil {
			return e
		}
		return nil
	}
	if e = check(); e != nil {
		return fail(e)
	}
	r.Eligible = true
	r.Reason = "48-hour retention elapsed; shipment and safety checks passed"
	if !apply {
		return r, nil
	}
	checkpoint, e := s.checkpointConversation(ctx, t)
	if e != nil {
		return fail(fmt.Errorf("archive checkpoint failed: %w", e))
	}
	t.Archive = &checkpoint
	if _, e = s.loadArchivedContext(t); e != nil {
		return fail(fmt.Errorf("archive context validation failed: %w", e))
	}
	// Re-run every guard after archiving, before publishing the deletion tombstone.
	if e = check(); e != nil {
		return fail(e)
	}
	checkpoint.Phase = "deleting"
	if e = s.Store.update(t.ID, func(v *Task) error { v.Archive = &checkpoint; return nil }); e != nil {
		return r, e
	}
	if sandbox {
		// Revalidate the durable bytes immediately before stopping maintenance. Once
		// the tombstone exists, any uncertain stop/delete remains operator-visible.
		if _, e = s.loadArchivedContext(t); e != nil {
			return fail(e)
		}
		started = false // This explicit stop is attempted once, including on uncertainty.
		if _, e = t.OpenShell.Config.call(ctx, "sandbox", "stop", t.OpenShell.Name); e != nil {
			return fail(fmt.Errorf("maintenance stop uncertain; archive retained: %w", e))
		}
		state, inspectErr := inspectTaskSandbox(ctx, t)
		if inspectErr != nil {
			return fail(fmt.Errorf("cannot confirm stopped sandbox ownership: %w", inspectErr))
		}
		if state.Phase != "Stopped" {
			return fail(fmt.Errorf("sandbox is not confirmed stopped; archive retained"))
		}
		if _, e = t.OpenShell.Config.call(ctx, "sandbox", "delete", t.OpenShell.Name); e != nil {
			return fail(fmt.Errorf("sandbox deletion uncertain; archive retained: %w", e))
		}
		if e = confirmSandboxAbsent(ctx, t); e != nil {
			return fail(fmt.Errorf("sandbox deletion unconfirmed; archive retained: %w", e))
		}
		deleted = true
	} else {
		if _, e = git(ctx, t.CommonDir, "-c", "core.hooksPath="+os.DevNull, "worktree", "remove", "--", t.Worktree); e != nil {
			return fail(e)
		}
		if _, e = os.Lstat(t.Worktree); !os.IsNotExist(e) {
			return fail(fmt.Errorf("workspace removal could not be confirmed"))
		}
	}
	checkpoint.Phase = "archived"
	if e = s.Store.update(t.ID, func(v *Task) error {
		v.Archive = &checkpoint
		v.WorkspaceCommit = t.ShippedCommit
		v.WorkspaceRemovedAt = now()
		return nil
	}); e != nil {
		return r, e
	}
	r.Removed = true
	r.Reason = "expired workspace; retained immutable task-linked context for a fresh conversation"
	return r, nil
}

// Accept only a structured complete list. Unknown formats and malformed entries
// cannot establish absence. A failed get is never evidence of deletion.
func confirmSandboxAbsent(ctx context.Context, t Task) error {
	if t.OpenShell == nil || t.OpenShell.Name == "" || t.OpenShell.ID == "" {
		return fmt.Errorf("missing sandbox identity for absence verification")
	}
	// NVIDIA OpenShell v0.1.6 Manage Sandboxes documents this paginated shape.
	token := ""
	seen := map[string]bool{}
	for page := 0; page < 1000; page++ {
		args := []string{"sandbox", "list", "--output", "json"}
		if token != "" {
			args = append(args, "--page-token", token)
		}
		b, e := t.OpenShell.Config.call(ctx, args...)
		if e != nil {
			return e
		}
		var response struct {
			Sandboxes     json.RawMessage `json:"sandboxes"`
			NextPageToken *string         `json:"next_page_token"`
		}
		if e = json.Unmarshal(b, &response); e != nil {
			return e
		}
		if len(response.Sandboxes) == 0 || !strings.HasPrefix(strings.TrimSpace(string(response.Sandboxes)), "[") || response.NextPageToken == nil {
			return fmt.Errorf("unknown or incomplete sandbox list schema")
		}
		var states []sandboxState
		if e = json.Unmarshal(response.Sandboxes, &states); e != nil {
			return e
		}
		for _, state := range states {
			if state.ID == "" || state.Name == "" {
				return fmt.Errorf("incomplete sandbox list identity")
			}
			if state.ID == t.OpenShell.ID || state.Name == t.OpenShell.Name || state.Labels["paddock.task"] == t.ID {
				return fmt.Errorf("task sandbox remains present")
			}
		}
		token = *response.NextPageToken
		if token == "" {
			return nil
		}
		if seen[token] {
			return fmt.Errorf("sandbox list pagination repeated a token")
		}
		seen[token] = true
	}
	return fmt.Errorf("sandbox list pagination limit exceeded")
}

func sandboxExpiryProcesses(ctx context.Context, t Task) error {
	probe := t
	probe.Worktree = "/sandbox"
	_, e := sandboxOutput(ctx, probe, "python3", "-c", sandboxExpiryProcessScript, t.Worktree)
	return e
}

const sandboxExpiryProcessScript = `import glob,os,sys
root=os.path.realpath(sys.argv[1])
def relevant(p):
 p=p.removesuffix(' (deleted)')
 return p==root or p.startswith(root+'/')
for proc in glob.glob('/proc/[0-9]*'):
 pid=int(proc.rsplit('/',1)[1])
 if pid==os.getpid(): continue
 try:
  paths=[os.readlink(proc+'/cwd')]
  paths += [os.readlink(fd) for fd in glob.glob(proc+'/fd/*')]
  with open(proc+'/maps') as f:
   paths += [line.split(None,5)[-1].strip() for line in f if len(line.split(None,5))==6]
  assert not any(relevant(p) for p in paths), 'workspace used by process '+str(pid)
 except FileNotFoundError: continue
`

// Known runtime data is disposable after explicit shipment. Unknown scratch at
// the workspace root remains protected; this does not widen ignored-file policy.
const sandboxExpiryScratchScript = `import os,stat
allowed={'worktree','repository.git','.paddock','go-cache','go-path','agent.lock','agent-exit.json','claude-process.json','idle-ready.json','idle-drain.json','idle-stopped.json','idle-rpc.sock','idle-control.sock'}
for name in os.listdir('/sandbox'):
 assert name in allowed, 'unknown sandbox scratch: '+name
 assert not stat.S_ISLNK(os.lstat('/sandbox/'+name).st_mode), 'symlink sandbox scratch: '+name
`
