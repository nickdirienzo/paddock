package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTPNativeCommandsRefreshRoleOnResume(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			task := taskFixture()
			task.Role, task.Runtime = "tp", "host"
			task.Session.Agent, task.Session.AgentPath = agent, agent
			task.Session.ConversationID = newConversationID()
			initial := AgentCommand(task, "/tmp/team state", "/tmp/paddock's cli")
			prompt := initial.Args[len(initial.Args)-1]
			if !strings.Contains(prompt, "all agents working on tasks") || strings.Contains(prompt, "Keep ordinary work in this task") {
				t.Fatal("TP received implementation instructions", prompt)
			}
			task.Session.Resume = true
			task.Session.ResumeNotice = "Restored workspace notice."
			resumed := AgentCommand(task, "/tmp/team state", "/tmp/paddock's cli")
			flag := "resume"
			if agent == "claude" {
				flag = "--resume"
			}
			want := []string{agent, flag, task.Session.ConversationID, "--", "Restored workspace notice.\n\n" + prompt}
			if !reflect.DeepEqual(resumed.Args, want) {
				t.Fatalf("resume changed conversation or lost role: %q", resumed.Args)
			}
		})
	}
}

func TestTPKeepsDurableWorkspaceOutOfGC(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Role = "tp"
	if err := s.Insert(task); err != nil {
		t.Fatal(err)
	}
	// No terminal, conversation, process, or Git queries should be needed.
	results, err := (Service{Store: s}).GC(context.Background(), true)
	if err != nil || len(results) != 1 || results[0].Removed || results[0].Eligible || !strings.Contains(results[0].Reason, "durable project context") {
		t.Fatal(results, err)
	}
}

func TestTPRejectsCorruptRecordsAndConcurrentOpen(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	dir := filepath.Join(s.Home, "tp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: s}
	for _, data := range []string{`broken`, `{"version":2}`, `{"version":1,"task_id":"../../outside","agent":"codex"}`} {
		if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.TeamPrincipal(context.Background(), TPOptions{}); err == nil {
			t.Fatal("accepted corrupt record", data)
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".launch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if ok, err := tryLock(lock); err != nil || !ok {
		t.Fatal(ok, err)
	}
	defer unlock(lock)
	if _, err := service.TeamPrincipal(context.Background(), TPOptions{}); err == nil || !strings.Contains(err.Error(), "already opening") {
		t.Fatal("concurrent TP opening was not rejected", err)
	}
	if tasks, err := s.List(""); err != nil || len(tasks) != 0 {
		t.Fatal("invalid opening created a task", tasks, err)
	}
}

func TestTPUncertainLaunchDoesNotCreateReplacement(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	t.Setenv("PADDOCK_CODEX_BIN", script(t, "exit 1"))
	t.Setenv("PADDOCK_SCREEN_BIN", script(t, `case "$1" in
 --version) echo 'Screen version fixture' ;;
 *) echo 'terminal unavailable' >&2; exit 1 ;;
esac`))
	service := Service{Store: s, Executable: "/tmp/paddock"}
	first, err := service.TeamPrincipal(context.Background(), TPOptions{Backend: "screen"})
	if err == nil || first.ID == "" || !strings.Contains(err.Error(), "uncertain") {
		t.Fatal("expected a durable task with uncertain launch", first, err)
	}
	again, err := service.TeamPrincipal(context.Background(), TPOptions{})
	if err == nil || again.ID != first.ID {
		t.Fatal("replaced an uncertain TP task", again, err)
	}
	tasks, err := s.List("")
	if err != nil || len(tasks) != 1 || tasks[0].Session.Agent != "codex" {
		t.Fatal(tasks, err)
	}
}

// Exercise the real CLI, supervisor, worktrees, delegation and native resume
// arguments with deterministic agent/terminal fixtures, without model calls.
func TestTPVerticalSlice(t *testing.T) {
	exe := buildCLI(t)
	home, control := t.TempDir(), t.TempDir()
	s := openTestStore(t, home)
	t.Setenv("PADDOCK_HOME", home)
	t.Setenv("PADDOCK_TASK_ID", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, err := filepath.EvalSymlinks(repoFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRepo(context.Background(), "mira", repo); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(control, "active")
	if err := os.Mkdir(active, 0700); err != nil {
		t.Fatal(err)
	}
	terminal := script(t, `case "$1" in
 --version) echo 'Screen version fixture' ;;
 -ls) found=0; for path in `+quote(active)+`/*; do
   if [ -f "$path" ]; then printf '123.%s (Detached)\n' "${path##*/}"; found=1; fi
   done; if [ "$found" = 0 ]; then echo 'No Sockets found in /tmp/screen.'; fi ;;
 -c) name=$4; shift 4; touch `+quote(active)+`/"$name"
   ( "$@"; rm -f `+quote(active)+`/"$name" ) </dev/null >`+quote(filepath.Join(control, "supervisor.log"))+` 2>&1 & ;;
 *) exit 1 ;;
esac`)
	t.Setenv("PADDOCK_SCREEN_BIN", terminal)
	agent := script(t, `if [ "${1:-}" = --version ]; then echo '2.1.286 (Claude Code)'; exit 0; fi
native_id=''; resumed=0
while [ "$#" -gt 0 ]; do
 case "$1" in
  --session-id) shift; native_id=$1 ;;
  --resume) shift; native_id=$1; resumed=1 ;;
 esac
 shift
done
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '%s\n' '{"type":"assistant"}' > "$CLAUDE_CONFIG_DIR/projects/fixture/$native_id.jsonl"
if [ -f AGENTS.md ]; then
 `+quote(exe)+` tasks --json > observed-tasks.json
 if [ "$resumed" = 0 ]; then
  mkdir -p projects/mira
  printf '%s\n' 'Decision: use the existing task runtime. Rationale: preserve project policy.' > projects/mira/decisions.md
 else
  cat projects/mira/decisions.md > recovered-decision
 fi
 while [ ! -f `+quote(filepath.Join(control, "finish"))+` ]; do
  if [ -f `+quote(filepath.Join(control, "delegate"))+` ] && [ ! -f delegated.json ]; then
   `+quote(exe)+` run --repo mira --agent claude --terminal screen --json 'Investigate the architecture decision' > delegated.json
  fi
  sleep 0.05
 done
fi
`)
	t.Setenv("PADDOCK_CLAUDE_BIN", agent)
	// Include historical work in another repository before TP exists.
	historical := taskFixture()
	historical.Status, historical.Repo = "stopped", repo
	historical.Session.TerminalPath = terminal
	if err := s.Insert(historical); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (Task, error) {
		t.Helper()
		cmd := exec.Command(exe, args...)
		cmd.Dir = control // no caller repository is needed
		data, err := cmd.CombinedOutput()
		if err != nil {
			return Task{}, fmt.Errorf("%w: %s", err, data)
		}
		var task Task
		err = json.Unmarshal(data, &task)
		return task, err
	}
	tp, err := invoke("tp", "--agent", "claude", "--terminal", "screen", "--json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(control, "finish"), nil, 0600)
		waitFor(t, func() bool {
			current, _ := s.Get(tp.ID)
			return current.Status == "stopped" || current.Status == "failed"
		})
	})
	if tp.Role != "tp" || tp.RuntimeName() != "host" || tp.ParentID != "" || tp.Status != "running" {
		t.Fatal(tp)
	}
	if runtime, err := s.DefaultRuntime(); err != nil || runtime != "openshell" {
		t.Fatal("TP changed the default implementation runtime", runtime, err)
	}
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(tp.Worktree, "projects/mira/decisions.md"))
		return err == nil
	})
	observed, err := os.ReadFile(filepath.Join(tp.Worktree, "observed-tasks.json"))
	if err != nil || !strings.Contains(string(observed), historical.ID) {
		t.Fatal("TP could not inspect historical team tasks", string(observed), err)
	}
	again, err := invoke("tp", "--json")
	if err != nil || again.ID != tp.ID || again.Session.Generation != tp.Session.Generation {
		t.Fatal("active TP was duplicated", again, err)
	}
	// Use ordinary run, with explicit project scope and normal runtime selection.
	if err := s.SetDefaultRuntime("host"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "delegate"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var child Task
	waitFor(t, func() bool {
		data, err := os.ReadFile(filepath.Join(tp.Worktree, "delegated.json"))
		return err == nil && json.Unmarshal(data, &child) == nil
	})
	if child.Role != "" || child.Repo != repo || child.RuntimeName() != "host" || child.CommonDir == tp.CommonDir {
		t.Fatal("delegation did not use normal project execution", child)
	}
	waitFor(t, func() bool { current, _ := s.Get(child.ID); return current.Status == "stopped" })
	if err := os.WriteFile(filepath.Join(control, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { current, _ := s.Get(tp.ID); return current.Status == "stopped" })
	waitFor(t, func() bool { entries, _ := os.ReadDir(active); return len(entries) == 0 })
	if err := os.Remove(filepath.Join(control, "finish")); err != nil {
		t.Fatal(err)
	}
	resumed, err := invoke("tp", "--json")
	if err != nil || resumed.ID != tp.ID || resumed.Worktree != tp.Worktree || resumed.Session.ConversationID != tp.Session.ConversationID || resumed.Session.Generation != tp.Session.Generation+1 {
		t.Fatal("TP did not resume the original conversation", resumed, err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(tp.Worktree, "recovered-decision")); return err == nil })
	if _, err := invoke("tp", "--agent", "codex", "--json"); err == nil {
		t.Fatal("silently changed TP agents")
	}
	tasks, err := s.List("")
	if err != nil || len(tasks) != 3 {
		t.Fatal("unexpected duplicate tasks", tasks, err)
	}
}
