package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOnlyConfirmedInitialMCPFailureCanReplayAssignment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Task)
		want   bool
	}{
		{"host Codex missing before first agent start", func(*Task) {}, true},
		{"host MCP grant blocked before first agent start", func(t *Task) {
			t.Error = "trusted host MCP tools are unavailable; LLM tool execution requires OpenShell"
		}, true},
		{"saved conversation must resume", func(t *Task) { t.Session.ConversationID = "saved-conversation" }, false},
		{"previous resume must not replay", func(t *Task) { t.Session.Resume = true }, false},
		{"missing history alone is insufficient", func(t *Task) { t.Error = "exit status 1" }, false},
		{"unrelated MCP failure", func(t *Task) { t.Error = "host MCP connection closed" }, false},
		{"host task", func(t *Task) { t.Runtime = "host" }, false},
		{"still starting", func(t *Task) { t.Status = "starting" }, false},
		{"running", func(t *Task) { t.Status = "running" }, false},
		{"legacy generation", func(t *Task) { t.Session.Generation = 0 }, false},
		{"no recorded exit", func(t *Task) { t.Session.ExitCode = nil }, false},
		{"different exit", func(t *Task) { code := 2; t.Session.ExitCode = &code }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code := 1
			task := Task{Runtime: "openshell", Status: "failed", Error: "trusted MCP bridge requires host Codex: exec: codex: executable file not found in $PATH",
				Session: Session{Generation: 1, ExitCode: &code}}
			tc.change(&task)
			if got := canRetryInitialMCPStartup(task); got != tc.want {
				t.Fatalf("retry initial assignment = %t; want %t", got, tc.want)
			}
		})
	}
}

func TestHostTaskCannotRestartThroughMessageOrLockedResume(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := Task{ID: newID(), Runtime: "host", Status: "stopped", Session: Session{Agent: "codex", ConversationID: newConversationID()}}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	if _, err := service.resumeLocked(context.Background(), task.ID); err == nil || !strings.Contains(err.Error(), "OpenShell") {
		t.Fatalf("locked resume accepted host task: %v", err)
	}
	if _, err := service.Message(context.Background(), task, "continue"); err == nil || !errors.Is(err, ErrAgentOffline) || !strings.Contains(err.Error(), "OpenShell") {
		t.Fatalf("message accepted host task: %v", err)
	}
	if state := service.WatchSessionState(context.Background(), task.ID); state != "unsupported" {
		t.Fatalf("host task advertised GitHub delivery state %q", state)
	}
}

func TestNativeResumeArgumentsAndHome(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			task := taskFixture()
			task.Prompt = "ORIGINAL ASSIGNMENT MUST NOT REPLAY"
			task.Session.Agent, task.Session.AgentPath = agent, agent
			task.Session.ConversationID = newConversationID()
			task.Session.AgentHome = "/tmp/native-agent-home"
			task.Session.Resume = true
			cmd := AgentCommand(task, "/tmp/registry", "/tmp/paddock")
			flag, homeKey := "resume", "CODEX_HOME="
			if agent == "claude" {
				flag, homeKey = "--resume", "CLAUDE_CONFIG_DIR="
			}
			want := []string{agent, flag, task.Session.ConversationID}
			if !reflect.DeepEqual(cmd.Args, want) {
				t.Fatalf("got %q want %q", cmd.Args, want)
			}
			if !strings.Contains(strings.Join(cmd.Env, "\n"), homeKey+task.Session.AgentHome) {
				t.Fatal("lost native home")
			}
			task.Session.ResumeNotice = "Workspace restored from Git; caches and uncommitted changes were not restored."
			cmd = AgentCommand(task, "/tmp/registry", "/tmp/paddock")
			if cmd.Args[len(cmd.Args)-1] != task.Session.ResumeNotice {
				t.Fatal(cmd.Args)
			}
		})
	}
}

func TestLiveNativeConversationLookup(t *testing.T) {
	id, cwd := os.Getenv("PADDOCK_LIVE_CONVERSATION"), os.Getenv("PADDOCK_LIVE_CONVERSATION_CWD")
	if id == "" || cwd == "" {
		t.Skip("set native conversation ID and cwd for a read-only installed-Codex check")
	}
	path, err := executable("PADDOCK_CODEX_BIN", "codex")
	if err != nil {
		t.Fatal(err)
	}
	got, err := codexTaskThread(context.Background(), Task{Worktree: cwd, Session: Session{Agent: "codex", AgentPath: path, AgentHome: os.Getenv("CODEX_HOME"), ConversationID: id}})
	if err != nil || got != id {
		t.Fatal(got, err)
	}
	got, err = codexTaskThread(context.Background(), Task{Worktree: cwd, Session: Session{Agent: "codex", AgentPath: path, AgentHome: os.Getenv("CODEX_HOME")}})
	if err != nil || got != id {
		t.Fatal("unsaved conversation recovery:", got, err)
	}
}

func TestCodexTaskThreadRecoversHostVscodeSession(t *testing.T) {
	cwd := t.TempDir()
	id := newConversationID()
	request := filepath.Join(t.TempDir(), "thread-list-request")
	agent := script(t, `while IFS= read -r line; do
 case "$line" in
  *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
  *'"id":2'*)
   printf '%s\n' "$line" > `+quote(request)+`
   printf '%s\n' '{"id":2,"result":{"data":[{"id":"`+id+`","cwd":"`+cwd+`"}],"nextCursor":null}}' ;;
 esac
done
`)
	task := Task{Runtime: "host", Worktree: cwd, Session: Session{Agent: "codex", AgentPath: agent}}
	got, err := codexTaskThread(context.Background(), task)
	if err != nil || got != id {
		t.Fatalf("recovered thread = %q, %v; want %q", got, err, id)
	}
	data, err := os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	var call struct {
		Params struct {
			SourceKinds []string `json:"sourceKinds"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &call); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(call.Params.SourceKinds, []string{"cli", "appServer", "vscode"}) {
		t.Fatalf("host source kinds = %q", call.Params.SourceKinds)
	}
}

func TestGitHubWakesSameConversationOnce(t *testing.T) {
	t.Skip("legacy host agent execution is disabled")
	s, task, _ := gcFixture(t)
	s.Executable = buildCLI(t)
	log, active, finish := filepath.Join(t.TempDir(), "messages"), filepath.Join(t.TempDir(), "active"), filepath.Join(t.TempDir(), "finish")
	agent := script(t, `case "$1" in
 app-server)
  while IFS= read -r line; do
   case "$line" in
    *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"id":2'*) printf '%s\n' '{"id":2,"result":{"thread":{"id":"`+task.Session.ConversationID+`","cwd":"`+canonicalWorkspacePath(task.Worktree)+`"},"data":[{"id":"`+task.Session.ConversationID+`","cwd":"`+canonicalWorkspacePath(task.Worktree)+`"}],"nextCursor":null}}' ;;
   esac
  done ;;
 queue) printf '%s\n' "$*" >> `+quote(log)+` ;;
 resume)
  while [ ! -f `+quote(finish)+` ]; do sleep 0.05; done
  rm -f `+quote(active)+` ;;
 *) exit 1 ;;
esac
`)
	terminal := script(t, `case "$1" in
 -ls) if [ -f `+quote(active)+` ]; then printf '123.%s (Detached)\n' "$(cat `+quote(active)+`)"; else printf 'No Sockets found in /tmp/screen.\n'; fi ;;
 -c) printf '%s' "$4" > `+quote(active)+`; shift 4; "$@" > /dev/null 2>&1 & ;;
 *) exit 1 ;;
esac
`)
	if err := s.Store.UpdateSession(task.ID, func(v *Session) { v.Agent, v.AgentPath, v.TerminalPath = "codex", agent, terminal }); err != nil {
		t.Fatal(err)
	}
	w := GitHubWatch{Repo: "team/repo", Number: 1, TaskID: task.ID, HeadRepo: "team/repo", HeadRef: task.Branch, Since: time.Now().Add(-time.Hour), Authors: map[string]int64{"engineer": 7}}
	if err := s.Store.UpdateGitHub(func(v *GitHubState) error {
		v.Identity = GitHubIdentity{Mode: "gh", Login: "engineer"}
		v.Watches[w.Key()] = w
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{}
	remote.pr.State, remote.pr.Head.Ref, remote.pr.Head.Repo.FullName, remote.pr.Base.Repo.FullName = "open", task.Branch, w.HeadRepo, w.Repo
	remote.comments = []GitHubComment{testComment(909, "continue the same task", 7)}
	g := GitHubCoordinator{Service: s, Remote: remote}
	t.Cleanup(func() {
		_ = os.WriteFile(finish, nil, 0600)
		waitFor(t, func() bool { v, _ := s.Store.Get(task.ID); return v.Status != "running" })
	})
	if state := s.WatchSessionState(context.Background(), task.ID); state != "resumable" {
		t.Fatal(state)
	}
	if events, err := g.Poll(context.Background()); err != nil || len(events) != 1 || events[0].Status != "done" {
		t.Fatal(events, err)
	}
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil || strings.Count(string(b), "queue --thread") != 1 || !strings.Contains(string(b), task.Session.ConversationID) {
		t.Fatalf("%q %v", b, err)
	}
	current, _ := s.Store.Get(task.ID)
	if current.Session.Generation != 1 || current.Session.ConversationID != task.Session.ConversationID {
		t.Fatal(current)
	}
}

func TestActivityClockAndStaleSupervisor(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	v := taskFixture()
	v.LastActivityAt = "2026-01-01T00:00:00Z"
	v.Session.Generation = 3
	if err := s.Insert(v); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSession(v.ID, func(s *Session) { s.ID = "metadata-only" }); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(v.ID)
	if got.LastActivityAt != v.LastActivityAt {
		t.Fatal("metadata refreshed retention")
	}
	old := 2
	if err := s.claimGeneration(v.ID, &old); err == nil {
		t.Fatal("stale supervisor claimed task")
	}
	current := 3
	if err := s.claimGeneration(v.ID, &current); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(v.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(v.ID)
	if got.Status != "stopped" || got.LastActivityAt == v.LastActivityAt {
		t.Fatal(got)
	}
}

func TestRecoveryAfterManualRemovalAndMovedBranch(t *testing.T) {
	s, task, checks := gcFixture(t)
	// Manual deletion already lost uncommitted state; reconstruct only Git.
	if err := os.RemoveAll(task.Worktree); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreWorktree(context.Background(), task)
	if err != nil || !restored {
		t.Fatal(restored, err)
	}
	if !gcOne(t, s, true, checks).Removed {
		t.Fatal("GC failed")
	}
	task, _ = s.Store.Get(task.ID)
	gitTest(t, task.Repo, "update-ref", "refs/heads/"+task.Branch, task.BaseCommit+"~0")
	// Record a mismatched checkpoint to simulate branch movement.
	task.WorkspaceCommit = strings.Repeat("f", 40)
	if _, err := restoreWorktree(context.Background(), task); err == nil {
		t.Fatal("ignored changed branch")
	}
}

func TestResumeSerializesAndKeepsWarmState(t *testing.T) {
	t.Skip("legacy host agent execution is disabled")
	s, task, _ := gcFixture(t)
	exe := buildCLI(t)
	s.Executable = exe
	log := filepath.Join(t.TempDir(), "resumed-args")
	active := filepath.Join(t.TempDir(), "active")
	finish := filepath.Join(t.TempDir(), "finish")
	// A real supervisor/process with fixture native CLI and terminal adapters.
	agent := script(t, `if [ "$1" = --version ]; then echo '2.1.286 (Claude Code)'; exit 0; fi
printf '%s\n' "$@" > `+quote(log)+`
while [ ! -f `+quote(finish)+` ]; do sleep 0.05; done
rm -f `+quote(active)+`
`)
	terminal := script(t, `case "$1" in
 -ls) if [ -f `+quote(active)+` ]; then printf '123.paddock-`+task.ID+`-1 (Detached)\n'; else printf 'No Sockets found in /tmp/screen.\n'; fi ;;
 -c) shift 4; touch `+quote(active)+`; "$@" > /dev/null 2>&1 & ;;
 *) exit 1 ;;
esac
`)
	if err := s.Store.UpdateSession(task.ID, func(v *Session) { v.AgentPath, v.TerminalPath = agent, terminal }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.Worktree, "warm-build-state"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	release, err := s.Store.taskLease(task.ID, "operation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(context.Background(), task.ID); err == nil {
		t.Fatal("concurrent operation allowed")
	}
	release()
	resumed, err := s.Resume(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(finish, nil, 0600)
		waitFor(t, func() bool { v, _ := s.Store.Get(task.ID); return v.Status == "stopped" })
	})
	if resumed.Status != "running" || resumed.Session.ConversationID != task.Session.ConversationID || resumed.Worktree != task.Worktree {
		t.Fatal(resumed)
	}
	waitFor(t, func() bool { _, err := os.Stat(log); return err == nil })
	b, err := os.ReadFile(log)
	if err != nil || !strings.HasPrefix(string(b), "--messaging-socket-path\n") || !strings.HasSuffix(string(b), "\n--resume\n"+task.Session.ConversationID+"\n") {
		t.Fatalf("%q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(task.Worktree, "warm-build-state")); err != nil || string(b) != "keep" {
		t.Fatal("lost warm files", err)
	}
	if again, err := s.Resume(context.Background(), task.ID); err != nil || again.Session.Generation != resumed.Session.Generation {
		t.Fatal("duplicate resume", again, err)
	}
}
