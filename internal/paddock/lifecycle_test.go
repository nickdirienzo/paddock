package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "paddock")
	c := exec.Command("go", "build", "-o", exe, "../../cmd/paddock")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	return exe
}
func script(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent with ' spaces")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nset -eu\n"+text), 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestSupervisorRecordsExitAndLiteralPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture requires Unix")
	}
	exe := buildCLI(t)
	s := openTestStore(t, t.TempDir())
	v := taskFixture()
	v.Worktree = t.TempDir()
	v.Prompt = "literal '; $(touch INJECTION); --help\nsecond line"
	v.Session.AgentPath = script(t, "printf '%s' \"$2\" > received\nprintf '%s' \"$PADDOCK_TASK_ID\" > identity\nexit 7\n")
	if e := s.Insert(v); e != nil {
		t.Fatal(e)
	}
	c := exec.Command(exe, "__run", v.ID)
	c.Env = append(os.Environ(), "PADDOCK_HOME="+s.Home)
	if b, e := c.CombinedOutput(); e == nil {
		t.Fatal("expected exit 7")
	} else if ee, ok := e.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		got, _ := s.Get(v.ID)
		t.Fatalf("%v: %s (recorded error: %s)", e, b, got.Error)
	}
	got, e := s.Get(v.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != "failed" || got.Session.ExitCode == nil || *got.Session.ExitCode != 7 {
		t.Fatalf("%+v", got)
	}
	b, e := os.ReadFile(filepath.Join(v.Worktree, "received"))
	if e != nil || !strings.HasPrefix(string(b), v.Prompt) {
		t.Fatalf("prompt changed: %q %v", b, e)
	}
	b, e = os.ReadFile(filepath.Join(v.Worktree, "identity"))
	if e != nil || string(b) != v.ID {
		t.Fatalf("identity: %q %v", b, e)
	}
	if _, e = os.Stat(filepath.Join(v.Worktree, "INJECTION")); !os.IsNotExist(e) {
		t.Fatal("prompt executed as shell")
	}
}

func TestAgentSpawnFailureIsDurable(t *testing.T) {
	exe := buildCLI(t)
	s := openTestStore(t, t.TempDir())
	v := taskFixture()
	v.Worktree = t.TempDir()
	v.Session.AgentPath = "/no/such/agent"
	if e := s.Insert(v); e != nil {
		t.Fatal(e)
	}
	c := exec.Command(exe, "__run", v.ID)
	c.Env = append(os.Environ(), "PADDOCK_HOME="+s.Home)
	_ = c.Run()
	got, e := s.Get(v.ID)
	if e != nil || got.Status != "failed" || got.Error == "" {
		t.Fatalf("%+v %v", got, e)
	}
}

// These tests use real persistent terminal servers and real Paddock subprocesses.
// Fixture agents keep the tests deterministic and avoid model calls or auth prompts.
func TestLiveTerminalLifecycle(t *testing.T) {
	backend := os.Getenv("PADDOCK_LIVE_TERMINAL")
	if backend == "" {
		t.Skip("set PADDOCK_LIVE_TERMINAL=rex or screen to exercise a real terminal")
	}
	t.Run("working-tree", func(t *testing.T) { liveTerminalLifecycle(t, backend, false) })
	t.Run("bare-repo-alias", func(t *testing.T) { liveTerminalLifecycle(t, backend, true) })
}

func liveTerminalLifecycle(t *testing.T, backend string, bareAlias bool) {
	exe := buildCLI(t)
	s := openTestStore(t, t.TempDir())
	repo := repoFixture(t)
	repoArgument := repo
	childAgent := "claude"
	if bareAlias {
		bare := filepath.Join(t.TempDir(), "mirage.git")
		gitTest(t, repo, "clone", "--bare", repo, bare)
		add := exec.Command(exe, "repo", "add", "mirage", bare)
		add.Env = append(os.Environ(), "PADDOCK_HOME="+s.Home)
		if b, err := add.CombinedOutput(); err != nil {
			t.Fatalf("register repo: %v %s", err, b)
		}
		repoArgument = "mirage"
		trust := exec.Command(exe, "repo", "trust", "mirage")
		trust.Env = add.Env
		if b, err := trust.CombinedOutput(); err != nil {
			t.Fatalf("trust repo: %v %s", err, b)
		}
		childAgent = "codex"
	}
	agent := script(t, `if [ "${1:-}" = "--version" ]; then exit 1; fi
printf '%s' "$PADDOCK_TASK_ID" > identity
printf '%s' "$$" > agent-pid
for argument; do prompt=$argument; done
case "$prompt" in
  parent*) `+quote(exe)+` delegate --runtime host --agent `+childAgent+` --terminal `+quote(backend)+` 'child task' --json > child.json ;;
esac
printf ready > ready
while [ ! -f finish ]; do sleep 0.1; done
exit 0
`)
	t.Setenv("PADDOCK_CODEX_BIN", agent)
	t.Setenv("PADDOCK_CLAUDE_BIN", agent)
	before := gitTest(t, repo, "status", "--porcelain=v1")
	head := gitTest(t, repo, "rev-parse", "HEAD")
	c := exec.Command(exe, "run", "parent task", "--runtime", "host", "--agent", "codex", "--terminal", backend, "--repo", repoArgument, "--json")
	c.Dir = t.TempDir() // Launch from outside any repository, as with a home directory.
	c.Env = append(os.Environ(), "PADDOCK_HOME="+s.Home)
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("run: %v %s", e, b)
	}
	var parent Task
	if e = json.Unmarshal(b, &parent); e != nil {
		t.Fatalf("%v %s", e, b)
	}
	t.Cleanup(func() {
		tasks, _ := s.List("")
		for _, v := range tasks {
			cleanupTerminal(v)
		}
	})
	var child Task
	waitFor(t, func() bool {
		b, e := os.ReadFile(filepath.Join(parent.Worktree, "child.json"))
		return e == nil && json.Unmarshal(b, &child) == nil
	})
	waitFor(t, func() bool { _, e := os.Stat(filepath.Join(child.Worktree, "ready")); return e == nil })
	// Parent and child were created by separate CLI processes, and now read by a third.
	show := exec.Command(exe, "show", parent.ID, "--json")
	show.Env = c.Env
	b, e = show.CombinedOutput()
	if e != nil {
		t.Fatalf("show: %v %s", e, b)
	}
	var d Detail
	if e = json.Unmarshal(b, &d); e != nil {
		t.Fatal(e)
	}
	if d.Status != "running" || d.Terminal.State != "running" || len(d.Children) != 1 || d.Children[0].ParentID != parent.ID || child.Session.Agent != childAgent || child.Worktree == parent.Worktree {
		t.Fatalf("bad delegation: %+v / %+v", d, child)
	}
	if parent.Session.TrustProject != bareAlias || child.Session.TrustProject != bareAlias {
		t.Fatalf("incorrect repository trust for parent/child: %+v / %+v", parent.Session, child.Session)
	}
	if backend == "rex" {
		for _, task := range []Task{parent, child} {
			term := terminal{path: task.Session.TerminalPath, endpoint: task.Session.Endpoint}
			b, err := term.rex(context.Background(), "block", "call", "-s", task.Session.ID, "-b", task.Session.BlockID, "process")
			if err != nil {
				t.Fatal(err)
			}
			var process struct {
				Child struct {
					PID int `json:"pid"`
				} `json:"child"`
				Foreground struct {
					PID int `json:"pid"`
				} `json:"foreground"`
			}
			if err = json.Unmarshal(b, &process); err != nil {
				t.Fatal(err)
			}
			pidText, err := os.ReadFile(filepath.Join(task.Worktree, "agent-pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(pidText))
			if err != nil || process.Foreground.PID != pid || process.Child.PID == pid {
				t.Fatalf("Rex should see the agent, not its supervisor, in the foreground: %s; expected pid %d", b, pid)
			}
		}
	}
	if gitTest(t, repo, "status", "--porcelain=v1") != before || gitTest(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("original checkout changed")
	}
	// Exit only the child: the parent's terminal and state must remain independent.
	if e = os.WriteFile(filepath.Join(child.Worktree, "finish"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	waitFor(t, func() bool { v, e := s.Get(child.ID); return e == nil && v.Status == "stopped" })
	p, e := s.Get(parent.ID)
	if e != nil || p.Status != "running" {
		t.Fatal(p, e)
	}
	if e = os.WriteFile(filepath.Join(parent.Worktree, "finish"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	waitFor(t, func() bool { v, e := s.Get(parent.ID); return e == nil && v.Status == "stopped" })
	service := Service{Store: s, Executable: exe}
	p, _, e = service.Refresh(context.Background(), p)
	if e != nil || p.Status != "stopped" {
		t.Fatal(p, e)
	}
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in 20 seconds")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
func cleanupTerminal(v Task) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if v.Session.Backend == "rex" {
		term := terminal{path: v.Session.TerminalPath, endpoint: v.Session.Endpoint}
		_, _ = term.rex(ctx, "kill", v.Session.ID)
	} else {
		_, _ = output(ctx, v.Session.TerminalPath, "-S", v.Session.Name, "-X", "quit")
	}
}
