package paddock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type GCResult struct {
	TaskID   string `json:"task_id"`
	Worktree string `json:"worktree"`
	Eligible bool   `json:"eligible"`
	Removed  bool   `json:"removed"`
	Reason   string `json:"reason"`
}

type gcChecks struct {
	processes func(context.Context, Task) error
	pulls     func(context.Context, Task) error
}

func (s Service) GC(ctx context.Context, apply bool) ([]GCResult, error) {
	return s.gc(ctx, apply, gcChecks{workspaceProcesses, s.noOpenPulls})
}

func (s Service) gc(ctx context.Context, apply bool, checks gcChecks) ([]GCResult, error) {
	config, err := s.Store.LifecycleConfig()
	if err != nil {
		return nil, err
	}
	ttl, _ := time.ParseDuration(config.WorktreeTTL)
	tasks, err := s.Store.List("")
	if err != nil {
		return nil, err
	}
	results := []GCResult{}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		result := GCResult{TaskID: task.ID, Worktree: task.Worktree}
		if task.Role == "tp" {
			result.Reason = "Team Principal workspace contains durable project context"
			results = append(results, result)
			continue
		}
		func() {
			release, err := s.Store.taskLease(task.ID, "operation")
			if err != nil {
				result.Reason = err.Error()
				return
			}
			defer release()
			t, err := s.Store.Get(task.ID)
			if err != nil {
				result.Reason = err.Error()
				return
			}
			if t.RuntimeName() == "openshell" {
				result = s.gcSandbox(ctx, t, config, apply)
				return
			}
			if t.Worktree != filepath.Join(s.Store.Home, "worktrees", t.ID) {
				result.Reason = "worktree is outside the managed task path"
				return
			}
			info, err := os.Lstat(t.Worktree)
			if errors.Is(err, os.ErrNotExist) {
				result.Reason = "worktree already absent"
				return
			}
			if err != nil {
				result.Reason = err.Error()
				return
			}
			if !info.IsDir() {
				result.Reason = "worktree is not a directory (or is a symlink)"
				return
			}
			// Legacy tasks lack a reliable activity clock. Initialize it on apply;
			// never guess they are old enough to delete on their first GC pass.
			if t.LastActivityAt == "" {
				result.Reason = "legacy task: begin a fresh retention period"
				if apply {
					if err := s.Store.TouchActivity(t.ID); err != nil {
						result.Reason = err.Error()
					}
				}
				return
			}
			activity, err := time.Parse(time.RFC3339Nano, t.LastActivityAt)
			if err != nil {
				result.Reason = "invalid activity timestamp"
				return
			}
			if time.Since(activity) < ttl {
				result.Reason = "within retention period"
				return
			}
			lease, err := s.Store.taskLease(t.ID, "process")
			if err != nil {
				result.Reason = err.Error()
				return
			}
			defer lease()
			if t.Status == "running" || t.Status == "starting" || t.Status == "preparing" {
				result.Reason = "task may still be running; reconcile with paddock show"
				return
			}
			_, obs := terminalFor(t.Session).Inspect(ctx, t)
			if obs.State != "missing" && obs.State != "exited" {
				result.Reason = "terminal process is active or unknown"
				return
			}
			check := func() error {
				if err := workspaceIdentity(ctx, t); err != nil {
					return err
				}
				if err := cleanWorkspace(ctx, t, config); err != nil {
					return err
				}
				if err := verifyConversation(ctx, t); err != nil {
					return fmt.Errorf("conversation is not recoverable: %w", err)
				}
				if err := s.noPendingWork(t.ID); err != nil {
					return err
				}
				if err := checks.processes(ctx, t); err != nil {
					return err
				}
				if err := pushedWorkspace(ctx, t); err != nil {
					return err
				}
				return checks.pulls(ctx, t)
			}
			if err = check(); err != nil {
				result.Reason = err.Error()
				return
			}
			result.Eligible, result.Reason = true, "expired and all safety checks passed"
			if !apply {
				return
			}
			head, err := taskGit(ctx, t, "rev-parse", "HEAD")
			if err != nil {
				result.Eligible = false
				result.Reason = err.Error()
				return
			}
			// Write the recovery checkpoint BEFORE removal, so a crash after Git's
			// successful removal still leaves everything required to reconstruct.
			if err = s.Store.update(t.ID, func(v *Task) error {
				v.WorkspaceCommit, v.WorkspaceRemovedAt = head, now()
				return nil
			}); err != nil {
				result.Eligible = false
				result.Reason = err.Error()
				return
			}
			// Recheck local mutable state immediately before removal. Git performs
			// its own dirty/locked-worktree check too. Never use --force or rm -rf.
			if err = cleanWorkspace(ctx, t, config); err == nil {
				err = checks.processes(ctx, t)
			}
			if err == nil {
				err = s.noPendingWork(t.ID)
			}
			if err == nil {
				current, e := taskGit(ctx, t, "rev-parse", "HEAD")
				if e != nil {
					err = e
				} else if current != head {
					err = fmt.Errorf("workspace changed during GC")
				}
			}
			if err == nil {
				_, err = git(ctx, t.CommonDir, "worktree", "remove", "--", t.Worktree)
			}
			if err != nil {
				result.Eligible, result.Reason = false, err.Error()
				return
			}
			result.Removed, result.Reason = true, "removed workspace; retained task, branch and native conversation"
		}()
		results = append(results, result)
	}
	return results, nil
}

func cleanWorkspace(ctx context.Context, t Task, config LifecycleConfig) error {
	flags, err := taskGit(ctx, t, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(flags, "\x00") {
		if len(entry) > 0 && (entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z') {
			return fmt.Errorf("index flags may hide local changes (skip-worktree or assume-unchanged)")
		}
	}
	status, err := taskGit(ctx, t, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("uncommitted or untracked changes")
	}
	// Git's normal removal discards ignored files; .env and other irreplaceable
	// ignored inputs are protected unless the operator names a disposable dir.
	ignored, err := taskGit(ctx, t, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, path := range strings.Split(ignored, "\x00") {
		if path == "" {
			continue
		}
		allowed := false
		for _, dir := range config.DisposableDirectories {
			if strings.HasPrefix(path, filepath.ToSlash(dir)+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("ignored files outside configured disposable directories")
		}
	}
	return nil
}

func pushedWorkspace(ctx context.Context, t Task) error {
	remote, err := taskGit(ctx, t, "config", "--get", "branch."+t.Branch+".remote")
	if err != nil || remote == "" || remote == "." {
		return fmt.Errorf("no remote upstream; cannot prove commits are pushed")
	}
	ref, err := taskGit(ctx, t, "config", "--get", "branch."+t.Branch+".merge")
	if err != nil || !strings.HasPrefix(ref, "refs/heads/") {
		return fmt.Errorf("no branch upstream; cannot prove commits are pushed")
	}
	// Query live remote refs, not a possibly stale origin/* tracking ref. A
	// missing/deleted upstream or unavailable ancestry retains the workspace.
	refs, err := taskGit(ctx, t, "ls-remote", "--exit-code", "--heads", "--", remote, ref)
	if err != nil {
		return fmt.Errorf("cannot verify remote commits: %w", err)
	}
	fields := strings.Fields(refs)
	if len(fields) != 2 || fields[1] != ref {
		return fmt.Errorf("remote upstream is ambiguous or missing")
	}
	if _, err = taskGit(ctx, t, "merge-base", "--is-ancestor", "HEAD", fields[0]); err != nil {
		return fmt.Errorf("unpushed commits or remote ancestry unavailable")
	}
	return nil
}

func (s Service) noPendingWork(id string) error {
	state, err := s.Store.GitHubState()
	if err != nil {
		return err
	}
	for _, e := range state.Events {
		if e.TaskID == id || slices.Contains(e.RecipientTaskIDs, id) {
			if e.Status != "done" && e.Status != "ignored" {
				return fmt.Errorf("pending or uncertain GitHub event")
			}
			for _, p := range state.Posts {
				if p.EventID == e.ID && p.Status != "posted" {
					return fmt.Errorf("pending GitHub reply")
				}
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(s.Store.Home, "signals", "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var r SignalRecord
		if err = json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Status != "done" && (r.Signal.TaskID == id || slices.Contains(r.Policy.Tasks, id)) {
			return fmt.Errorf("pending or uncertain orchestration signal")
		}
	}
	return nil
}

// A process lease covers Paddock's supervisor. lsof additionally catches detached
// builds, editors, shells and open files. Missing support/permission => retain.
func workspaceProcesses(ctx context.Context, t Task) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.Worktree)
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, "lsof", "-nP", "-Fpn")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	b, err := c.Output()
	if err != nil || stderr.Len() != 0 {
		return fmt.Errorf("cannot verify workspace process use (lsof unavailable or incomplete)")
	}
	pid := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "p") {
			pid = line[1:]
		}
		if !strings.HasPrefix(line, "n") {
			continue
		}
		path := strings.TrimSuffix(line[1:], " (deleted)")
		if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) || path == t.Worktree || strings.HasPrefix(path, t.Worktree+string(os.PathSeparator)) {
			return fmt.Errorf("workspace is used by process %s", pid)
		}
	}
	return nil
}
