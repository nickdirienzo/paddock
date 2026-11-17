package paddock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archivedHostFixture(t *testing.T) (Service, Task, gcChecks) {
	t.Helper()
	s, task, checks := gcFixture(t)
	checkpoint, err := s.checkpointConversation(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Phase = "archived"
	if err = s.Store.update(task.ID, func(v *Task) error { v.Archive = &checkpoint; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = git(context.Background(), task.Repo, "worktree", "remove", "--", task.Worktree); err != nil {
		t.Fatal(err)
	}
	task, err = s.Store.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, checks
}

func TestReviveNewConversationOnFreshMainWithContext(t *testing.T) {
	s, task, _ := archivedHostFixture(t)
	// Simulate main advancing after the original work shipped.
	gitTest(t, task.Repo, "push", "gc-origin", "HEAD:refs/heads/main")
	gitTest(t, task.Repo, "remote", "add", "origin", gitTest(t, task.Repo, "remote", "get-url", "gc-origin"))
	s.Executable = buildCLI(t)
	log := filepath.Join(t.TempDir(), "agent-args")
	active := filepath.Join(t.TempDir(), "active")
	finish := filepath.Join(t.TempDir(), "finish")
	agent := script(t, `if [ "$1" = --version ]; then echo '2.1.325 (Claude Code)'; exit 0; fi
printf '%s\n' "$@" > `+quote(log)+`
while [ ! -f `+quote(finish)+` ]; do sleep 0.05; done
`)
	terminal := script(t, `case "$1" in
 -ls) if [ -f `+quote(active)+` ]; then printf '123.paddock-`+task.ID+`-1 (Detached)\n'; else printf 'No Sockets found in /tmp/screen.\n'; fi ;;
 -c) shift 4; touch `+quote(active)+`; "$@" > /dev/null 2>&1 & ;;
 *) exit 1;;
esac`)
	if err := s.Store.UpdateSession(task.ID, func(v *Session) { v.AgentPath = agent; v.TerminalPath = terminal }); err != nil {
		t.Fatal(err)
	}
	sourceHead := gitTest(t, task.Repo, "rev-parse", "HEAD")
	revived, err := s.Revive(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(finish, nil, 0600)
		waitFor(t, func() bool { current, _ := s.Store.Get(task.ID); return current.Status == "stopped" })
	})
	if revived.ID != task.ID || revived.Session.Resume || revived.Session.ConversationID == task.Session.ConversationID || revived.BaseCommit != sourceHead || revived.Base != "origin/main" || revived.Branch == task.Branch {
		t.Fatal(revived)
	}
	if revived.Archive.Phase != "revived" || revived.ShippedAt != "" || revived.ShippedCommit != "" {
		t.Fatal("revival still qualified as shipped")
	}
	waitFor(t, func() bool { _, err := os.Stat(log); return err == nil })
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "--resume") || strings.Contains(string(args), task.Session.ConversationID) {
		t.Fatal("used old native conversation", string(args))
	}
	for _, text := range []string{"NEW native conversation", "fresh origin/main", revived.Session.ContextFile, "wait for the user's next instruction"} {
		if !strings.Contains(string(args), text) {
			t.Fatalf("missing revival instruction %q", text)
		}
	}
	b, err := os.ReadFile(revived.Session.ContextFile)
	if err != nil || !strings.Contains(string(b), "fixture conversation") {
		t.Fatal("lost chat context", err)
	}
	if _, err := s.loadArchivedContext(revived); err != nil {
		t.Fatal("archive invalidated by new generation", err)
	}
	tasks, err := s.Store.List("")
	if err != nil || len(tasks) != 1 {
		t.Fatal("created extra Paddock tasks", err)
	}
}

func TestReviveRejectsUnknownLifecycleAndBrokenArchive(t *testing.T) {
	for _, which := range []string{"deleting", "reviving", "corrupt", "missing", "active-process", "operation-lease", "unknown-terminal"} {
		t.Run(which, func(t *testing.T) {
			s, task, _ := archivedHostFixture(t)
			switch which {
			case "deleting", "reviving":
				_ = s.Store.update(task.ID, func(v *Task) error { v.Archive.Phase = which; return nil })
			case "corrupt":
				_ = os.WriteFile(filepath.Join(s.Store.Home, "archives", task.Archive.File), []byte("broken"), 0600)
			case "missing":
				_ = os.Rename(filepath.Join(s.Store.Home, "archives", task.Archive.File), filepath.Join(s.Store.Home, "archives", "missing.json"))
			case "active-process", "operation-lease":
				kind := "process"
				if which == "operation-lease" {
					kind = "operation"
				}
				release, err := s.Store.taskLease(task.ID, kind)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "unknown-terminal":
				_ = s.Store.UpdateSession(task.ID, func(v *Session) { v.TerminalPath = script(t, "exit 2") })
			}
			before, _ := os.ReadFile(s.Store.path(task.ID))
			if _, err := s.Revive(context.Background(), task.ID); err == nil {
				t.Fatal("unsafe revival")
			}
			after, _ := os.ReadFile(s.Store.path(task.ID))
			if string(before) != string(after) {
				t.Fatal("failed preflight mutated task")
			}
		})
	}
}

func TestRevivalContextIntegrityAndRepeatedArchives(t *testing.T) {
	s, task, _ := gcFixture(t)
	first, err := s.checkpointConversation(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	task.Archive = &first
	text, err := s.loadArchivedContext(task)
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.writeRevivalContext(task.ID, 1, text)
	if err != nil {
		t.Fatal(err)
	}
	task.Session.ContextFile = path
	task.Session.ContextSHA256 = digest([]byte(text))
	if err = s.verifyRevivalContext(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.verifyRevivalContext(context.Background(), task); err == nil {
		t.Fatal("corrupt context accepted")
	}
	// A fresh native archive includes historical context from the checkpoint,
	// without depending on the prior home, workspace, or mutable latest manifest.
	task.Session.ContextFile = path
	a, err := s.archiveConversation(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.ContextText, "LATER CONVERSATION") || !strings.Contains(a.ContextText, text) {
		t.Fatal("lost preceding context")
	}
	if _, err := s.loadArchivedContext(task); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(s.Store.Home, "archives", first.File), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadArchivedContext(task); err == nil {
		t.Fatal("public archive accepted")
	}
}

func TestRevivalPromptDoesNotReplayOriginalGoal(t *testing.T) {
	task := taskFixture()
	task.Prompt = "/goal Delete the old deployment"
	task.BaseCommit = strings.Repeat("f", 40)
	task.Session.ContextFile = "/private/context.md"
	prompt := agentPrompt(task, "/registry", "/paddock")
	if strings.Contains(prompt, task.Prompt) || !strings.Contains(prompt, "historical context") {
		t.Fatal(prompt)
	}
}
