//go:build darwin || linux

package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Exercise the real Codex goal store and interactive resume against an isolated
// native home and a local model. Never use host login or external model calls.
func TestNativeCodexGoalLaunch(t *testing.T) {
	agent := os.Getenv("PADDOCK_NATIVE_CODEX")
	if agent == "" {
		t.Skip("set PADDOCK_NATIVE_CODEX to test a real Codex binary")
	}
	for _, objective := range []string{"Finish the fixture objective", "# Finish the fixture objective\n\n" + strings.Repeat("Preserve all scope and validation requirements. ", 120) + "\nLONG_GOAL_END"} {
		t.Run(fmt.Sprint(len(objective)), func(t *testing.T) { testNativeCodexGoalLaunch(t, agent, objective) })
	}
}

func testNativeCodexGoalLaunch(t *testing.T, agent, objective string) {
	home, worktree := t.TempDir(), t.TempDir()
	requests := make(chan string, 10)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "responses") {
			requests <- string(body)
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer model.Close()
	config := fmt.Sprintf("model = \"fixture\"\nmodel_provider = \"fixture\"\n[features]\ngoals = true\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n", model.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := taskFixture()
	task.Prompt, task.Runtime, task.Worktree = "/goal "+objective, "host", worktree
	task.Session = Session{Agent: "codex", AgentPath: agent, AgentHome: home, TrustProject: true}
	if err = store.Insert(task); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = prepareInitialGoal(ctx, store, &task, "/fixture/paddock"); err != nil {
		t.Fatal(err)
	}
	if !task.Session.GoalInitialized || !conversationUUID.MatchString(task.Session.ConversationID) {
		t.Fatal(task.Session)
	}
	select {
	case request := <-requests:
		t.Fatalf("goal preparation started model work: %s", request)
	default:
	}
	command := AgentCommand(task, store.Home, "/fixture/paddock")
	command.Env = append(command.Env, "TERM=xterm-256color")
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		// Stop only the disposable fixture home's native daemon.
		stop := exec.Command(agent, "app-server", "daemon", "stop")
		stop.Env = command.Env
		_ = stop.Run()
	}()
	go io.Copy(io.Discard, terminal)
	select {
	case request := <-requests:
		encoded, _ := json.Marshal(objective)
		if !strings.Contains(request, string(encoded[1:len(encoded)-1])) || !strings.Contains(request, "Paddock context") {
			t.Fatalf("goal/context missing from native turn: %s", request)
		}
	case <-ctx.Done():
		t.Fatal("native goal did not start when the TUI resumed the prepared thread")
	}
	rpc, err := openGoalRPC(ctx, task, store.Home, "/fixture/paddock")
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.close()
	var response struct {
		Goal *nativeGoal `json:"goal"`
	}
	if err = rpc.call("thread/goal/get", map[string]any{"threadId": task.Session.ConversationID}, &response); err != nil {
		t.Fatal(err)
	}
	if response.Goal == nil || response.Goal.Objective != nativeGoalObjective(objective) || response.Goal.Status != "active" {
		t.Fatalf("not an actual native goal: %+v", response.Goal)
	}
}

func TestNativeCodexGoalPreparationBeforeLogin(t *testing.T) {
	agent := os.Getenv("PADDOCK_NATIVE_CODEX")
	if agent == "" {
		t.Skip("set PADDOCK_NATIVE_CODEX to test a real Codex binary")
	}
	store := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Prompt, task.Runtime, task.Worktree = "/goal Prepare without running model work", "host", t.TempDir()
	task.Session = Session{Agent: "codex", AgentPath: agent, AgentHome: t.TempDir(), TrustProject: true}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	if err := prepareInitialGoal(context.Background(), store, &task, "/fixture/paddock"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLongGoalPreparation(t *testing.T) {
	agent := os.Getenv("PADDOCK_NATIVE_CODEX")
	if agent == "" {
		t.Skip("set PADDOCK_NATIVE_CODEX to test a real Codex binary")
	}
	store := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Prompt, task.Runtime, task.Worktree = "/goal "+strings.Repeat("x", 5200), "host", t.TempDir()
	task.Session = Session{Agent: "codex", AgentPath: agent, AgentHome: t.TempDir(), TrustProject: true}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	if err := prepareInitialGoal(context.Background(), store, &task, "/fixture/paddock"); err != nil {
		t.Fatal(err)
	}
}
