package paddock

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gcFixture(t *testing.T) (Service, Task, gcChecks) {
	t.Helper()
	s := openTestStore(t, t.TempDir())
	repo := repoFixture(t)
	r, err := ResolveRepository(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	v := taskFixture()
	v.Repo, v.CommonDir, v.BaseCommit = r.Root, r.CommonDir, r.Commit
	v.Branch = "paddock/" + v.ID
	v.Worktree = filepath.Join(s.Home, "worktrees", v.ID)
	v.LastActivityAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	v.Status = "stopped"
	v.Session.Agent, v.Session.AgentHome, v.Session.ConversationID = "claude", t.TempDir(), newConversationID()
	transcript := filepath.Join(v.Session.AgentHome, "projects", "test", v.Session.ConversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{\"type\":\"assistant\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v.Session.TerminalPath = script(t, "echo 'No Sockets found in /tmp/screen.'; exit 1\n")
	if err := s.Insert(v); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitTest(t, repo, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "gc-origin", remote)
	gitTest(t, v.Worktree, "push", "-u", "gc-origin", "HEAD")
	ok := func(context.Context, Task) error { return nil }
	return Service{Store: s}, v, gcChecks{ok, ok}
}

func gcOne(t *testing.T, s Service, apply bool, checks gcChecks) GCResult {
	t.Helper()
	results, err := s.gc(context.Background(), apply, checks)
	if err != nil || len(results) != 1 {
		t.Fatalf("GC: %+v %v", results, err)
	}
	return results[0]
}

func TestGCRetainsWarmWorkspaceThenRecoversTaskHead(t *testing.T) {
	s, task, checks := gcFixture(t)
	// Recovery must retain task commits, not revert to the original base.
	if err := os.WriteFile(filepath.Join(task.Worktree, "committed"), []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, task.Worktree, "add", "committed")
	gitTest(t, task.Worktree, "commit", "-m", "task work")
	gitTest(t, task.Worktree, "push")
	head := gitTest(t, task.Worktree, "rev-parse", "HEAD")
	before, _ := os.ReadFile(s.Store.path(task.ID))
	preview := gcOne(t, s, false, checks)
	if !preview.Eligible || preview.Removed {
		t.Fatal(preview)
	}
	after, _ := os.ReadFile(s.Store.path(task.ID))
	if string(before) != string(after) {
		t.Fatal("preview modified task")
	}
	result := gcOne(t, s, true, checks)
	if !result.Removed {
		t.Fatal(result)
	}
	if _, err := os.Stat(task.Worktree); !os.IsNotExist(err) {
		t.Fatal("workspace remains", err)
	}
	current, err := s.Store.Get(task.ID)
	if err != nil || current.WorkspaceCommit != head || current.Session.ConversationID != task.Session.ConversationID {
		t.Fatal(current, err)
	}
	if err := verifyConversation(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreWorktree(context.Background(), current)
	if err != nil || !restored {
		t.Fatal(restored, err)
	}
	if got := gitTest(t, task.Worktree, "rev-parse", "HEAD"); got != head || got == task.BaseCommit {
		t.Fatal("restored wrong commit", got)
	}
	if err := s.Store.TouchActivity(task.ID); err != nil {
		t.Fatal(err)
	}
	if result := gcOne(t, s, false, checks); result.Eligible || !strings.Contains(result.Reason, "retention") {
		t.Fatal(result)
	}
}

func TestGCProtectionGates(t *testing.T) {
	for _, name := range []string{"dirty", "untracked", "ignored", "assume-unchanged", "skip-worktree", "unpushed", "deleted-upstream", "active-process", "unknown-process", "open-pr", "unknown-pr", "process-lease", "pending-signal", "locked-worktree", "legacy"} {
		t.Run(name, func(t *testing.T) {
			s, task, checks := gcFixture(t)
			switch name {
			case "assume-unchanged", "skip-worktree":
				path := filepath.Join(task.Worktree, "tracked")
				if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, task.Worktree, "add", "tracked")
				gitTest(t, task.Worktree, "commit", "-m", "tracked input")
				gitTest(t, task.Worktree, "push")
				gitTest(t, task.Worktree, "update-index", "--"+name, "--", "tracked")
				if err := os.WriteFile(path, []byte("hidden edits"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				if err := os.WriteFile(filepath.Join(task.Worktree, "dirty"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, task.Worktree, "add", "dirty")
			case "untracked":
				if err := os.WriteFile(filepath.Join(task.Worktree, "untracked"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ignored":
				gitTest(t, task.Worktree, "config", "core.excludesFile", filepath.Join(t.TempDir(), "ignore"))
				p := gitTest(t, task.Worktree, "config", "core.excludesFile")
				if err := os.WriteFile(p, []byte(".env\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(task.Worktree, ".env"), []byte("secret input"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unpushed":
				gitTest(t, task.Worktree, "commit", "--allow-empty", "-m", "not pushed")
			case "deleted-upstream":
				gitTest(t, task.Worktree, "push", "gc-origin", "--delete", task.Branch)
			case "active-process", "unknown-process":
				checks.processes = func(context.Context, Task) error { return fmt.Errorf("%s", name) }
			case "open-pr", "unknown-pr":
				checks.pulls = func(context.Context, Task) error { return fmt.Errorf("%s", name) }
			case "process-lease":
				release, err := s.Store.taskLease(task.ID, "process")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "pending-signal":
				path := filepath.Join(s.Store.Home, "signals")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := writeJSONAtomic(filepath.Join(path, "signal.json"), SignalRecord{Signal: Signal{TaskID: task.ID}, Status: "pending"}); err != nil {
					t.Fatal(err)
				}
			case "locked-worktree":
				gitTest(t, task.Repo, "worktree", "lock", task.Worktree)
			case "legacy":
				if err := s.Store.update(task.ID, func(v *Task) error { v.LastActivityAt = ""; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			result := gcOne(t, s, true, checks)
			if result.Removed {
				t.Fatal("unsafe removal", result)
			}
			if _, err := os.Stat(task.Worktree); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGCDisposableArtifactsAreExplicit(t *testing.T) {
	s, task, checks := gcFixture(t)
	if err := os.WriteFile(filepath.Join(task.Worktree, ".gitignore"), []byte("build/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, task.Worktree, "add", ".gitignore")
	gitTest(t, task.Worktree, "commit", "-m", "ignore build")
	gitTest(t, task.Worktree, "push")
	if err := os.Mkdir(filepath.Join(task.Worktree, "build"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.Worktree, "build", "cache"), []byte("expensive"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := gcOne(t, s, false, checks); result.Eligible {
		t.Fatal("ignored state discarded by default")
	}
	if err := s.Store.SetLifecycleConfig(LifecycleConfig{WorktreeTTL: "168h", DisposableDirectories: []string{"build"}}); err != nil {
		t.Fatal(err)
	}
	if result := gcOne(t, s, true, checks); !result.Removed {
		t.Fatal(result)
	}
}

func TestWorkspaceProcessGuard(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof unavailable")
	}
	task := Task{Worktree: t.TempDir()}
	cmd := exec.Command("sleep", "30")
	cmd.Dir = task.Worktree
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if err := workspaceProcesses(context.Background(), task); err == nil {
		t.Fatal("missed process with worktree cwd")
	}
}
