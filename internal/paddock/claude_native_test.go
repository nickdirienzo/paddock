//go:build darwin || linux

package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Qualify the real interactive CLI against a local model and an isolated home.
// No host login, external model calls, or existing sessions are used.
func TestNativeClaudeInboxAndResume(t *testing.T) {
	agent := os.Getenv("PADDOCK_NATIVE_CLAUDE")
	if agent == "" {
		t.Skip("set PADDOCK_NATIVE_CLAUDE to a qualified Claude binary")
	}
	for _, ttl := range []string{"", "1h"} {
		t.Run("ttl="+ttl, func(t *testing.T) { testNativeClaudeInboxAndResume(t, agent, ttl) })
	}
}

func testNativeClaudeInboxAndResume(t *testing.T, agent, ttl string) {
	if ttl != "" {
		version, err := exec.Command(agent, "--version").Output()
		if err != nil || strings.TrimSpace(string(version)) != claudeInboxVersion {
			t.Skip("Claude TTL launches require the new 2.1.325 runtime")
		}
	}
	// Keep runtime control sockets below macOS's Unix socket path limit.
	root, err := os.MkdirTemp("/tmp", "pd-ci-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	home, worktree := filepath.Join(root, "home"), filepath.Join(root, "worktree")
	for _, path := range []string{home, worktree} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := map[string]any{"hasCompletedOnboarding": true, "theme": "dark", "messageIdleNotifThresholdMs": 100,
		"customApiKeyResponses": map[string]any{"approved": []string{"paddock-fixture"}},
		"projects":              map[string]any{worktree: map[string]any{"hasTrustDialogAccepted": true}}}
	if err := writeJSONAtomic(filepath.Join(home, ".claude.json"), config); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(home, "settings.json"), map[string]any{
		"permissions": map[string]any{"ask": []string{"Bash"}},
	}); err != nil {
		t.Fatal(err)
	}
	var approval atomic.Bool
	var sawBash, sawAgent atomic.Bool
	var holdModel atomic.Bool
	modelStarted, modelRelease := make(chan struct{}, 1), make(chan struct{})
	approvalMarker := filepath.Join(worktree, "approval-marker")
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "count_tokens") {
			io.WriteString(w, `{"input_tokens":1}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"input_schema"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Stream && holdModel.Swap(false) {
			modelStarted <- struct{}{}
			select {
			case <-modelRelease:
			case <-r.Context().Done():
				return
			}
		}
		for _, tool := range request.Tools {
			if tool.Name != "Bash" && tool.Name != "Agent" && tool.Name != "Task" {
				continue
			}
			if tool.InputSchema.Properties == nil {
				continue
			}
			if tool.Name == "Bash" {
				sawBash.Store(true)
			} else {
				sawAgent.Store(true)
			}
			if ttl != "" && tool.InputSchema.Properties["run_in_background"] != nil {
				t.Errorf("TTL launch still advertises background execution on %s", tool.Name)
			}
		}
		message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant",
			"model": "claude-opus-4-8", "content": []any{}, "stop_reason": nil,
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
		if !request.Stream {
			message["content"] = []any{map[string]string{"type": "text", "text": "PADDOCK_FIXTURE_OK"}}
			message["stop_reason"] = "end_turn"
			json.NewEncoder(w).Encode(message)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, fields map[string]any) {
			fields["type"] = kind
			data, _ := json.Marshal(fields)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		}
		event("message_start", map[string]any{"message": message})
		stopReason := "end_turn"
		if approval.Swap(false) {
			stopReason = "tool_use"
			event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use", "id": "tool_fixture", "name": "Bash", "input": map[string]any{}}})
			input, _ := json.Marshal(map[string]string{"command": "touch " + quote(approvalMarker), "description": "Create a disposable approval fixture"})
			event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(input)}})
		} else {
			event("content_block_start", map[string]any{"index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
			event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "text_delta", "text": "PADDOCK_FIXTURE_OK"}})
		}
		event("content_block_stop", map[string]any{"index": 0})
		event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
		event("message_stop", map[string]any{})
	}))
	defer model.Close()
	task := taskFixture()
	task.Worktree = worktree
	task.IdleTTL = ttl
	task.Session = Session{Agent: "claude", AgentPath: agent, AgentHome: home,
		ConversationID: newConversationID(), Generation: 1}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + home,
		"ANTHROPIC_API_KEY=paddock-fixture", "ANTHROPIC_BASE_URL=" + model.URL,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_EXIT_AFTER_STOP_DELAY=100",
		"CLAUDE_CODE_DISABLE_TERMINAL_TITLE=1", "TERM=xterm-256color"}
	env = append(env, claudeIdleEnv(task)...)
	if ttl != "" {
		env = append(env, "PADDOCK_CONTROL_DIR="+root, "PADDOCK_IDLE_TTL="+ttl)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if ttl != "" {
		command := exec.CommandContext(ctx, agent, "--bg", "--", "Do not start a background session")
		command.Dir, command.Env = worktree, env
		data, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(data), "CLAUDE_CODE_DISABLE_AGENT_VIEW") {
			t.Fatalf("TTL launch did not refuse a background session: %s (%v)", data, err)
		}
	}
	var terminalLog string
	idleRequest := func(operation string, checkpoint map[string]any) map[string]any {
		t.Helper()
		conn, err := net.DialTimeout("unix", filepath.Join(root, "idle-control.sock"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		request := map[string]any{"op": operation, "generation": task.Session.Generation,
			"thread": task.Session.ConversationID, "cwd": worktree}
		for _, key := range []string{"epoch", "turn_id"} {
			if checkpoint != nil {
				request[key] = checkpoint[key]
			}
		}
		if err := json.NewEncoder(conn).Encode(request); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.NewDecoder(conn).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	deliver := func(inbox claudeInbox, message string) {
		t.Helper()
		if helper := os.Getenv("PADDOCK_NATIVE_MESSAGE_HELPER"); ttl != "" && helper != "" {
			command := exec.CommandContext(ctx, "python3", helper, task.Session.ConversationID, fmt.Sprint(task.Session.Generation))
			command.Env, command.Dir = env, worktree
			body, _ := json.Marshal(map[string]string{"message": message})
			command.Stdin = strings.NewReader(string(body))
			data, err := command.Output()
			if err != nil || strings.TrimSpace(string(data)) != "queued" {
				t.Fatalf("runtime inbox delivery failed: %s (%v)", data, err)
			}
			return
		}
		if ttl != "" && idleRequest("delivery", nil)["state"] != "admitted" {
			t.Fatal("runtime did not admit delivery")
		}
		if _, err := deliverHostClaude(ctx, task, inbox, message); err != nil {
			t.Fatal(err)
		}
	}
	start := func(resume bool) (claudeInbox, func()) {
		t.Helper()
		args := []string{"--session-id", task.Session.ConversationID, "--", "Reply PADDOCK_FIXTURE_OK"}
		if resume {
			args = []string{"--resume", task.Session.ConversationID}
		}
		command := exec.Command(agent, args...)
		command.Dir, command.Env = worktree, env
		inbox, cleanup, err := prepareHostClaudeInbox(task, command)
		if err != nil {
			t.Fatal(err)
		}
		if ttl != "" {
			if launcher := os.Getenv("PADDOCK_NATIVE_LAUNCHER"); launcher != "" {
				// The production launcher owns its private native inbox path.
				nativeArgs := append([]string{command.Args[0]}, command.Args[3:]...)
				command = exec.Command("python3", append([]string{launcher, fmt.Sprint(task.Session.Generation)}, nativeArgs...)...)
				inbox.Socket = filepath.Join(home, "paddock-inbox.sock")
			} else {
				helper, err := filepath.Abs("../../runtime/openshell/paddock-claude-idle")
				if err != nil {
					t.Fatal(err)
				}
				command = exec.Command("python3", append([]string{helper, "launch", fmt.Sprint(task.Session.Generation)}, command.Args...)...)
			}
			command.Dir, command.Env = worktree, env
		}
		terminal, err := pty.Start(command)
		if err != nil {
			cleanup()
			t.Fatal(err)
		}
		log, err := os.CreateTemp(root, "terminal-")
		if err != nil {
			t.Fatal(err)
		}
		terminalLog = log.Name()
		readDone := make(chan struct{})
		go func() { defer close(readDone); io.Copy(log, terminal) }()
		inbox.PID = command.Process.Pid
		if ttl != "" {
			for ctx.Err() == nil {
				data, _ := os.ReadFile(filepath.Join(root, "claude-process.json"))
				var native claudeInbox
				if json.Unmarshal(data, &native) == nil && native.Generation == task.Session.Generation && native.PID > 0 {
					inbox.PID = native.PID
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		return inbox, func() {
			if ttl != "" {
				if native, err := os.FindProcess(inbox.PID); err == nil {
					native.Signal(syscall.SIGTERM) // disposable native fixture only
				}
			} else {
				command.Process.Signal(syscall.SIGTERM)
			}
			done := make(chan struct{})
			go func() { command.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				command.Process.Kill() // disposable fixture only
				<-done
			}
			terminal.Close()
			<-readDone
			log.Close()
			cleanup()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Logf("Claude terminal: %s", data)
			}
		}
	}
	completed := func(count int) {
		t.Helper()
		for ctx.Err() == nil {
			command := exec.CommandContext(ctx, "python3", "-c", claudeOutputScript, home, task.Session.ConversationID, worktree)
			data, err := command.Output()
			if err == nil && strings.Count(string(data), `"type": "turn_finished"`) >= count {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("native Claude did not persist completed turns")
	}
	inbox, stop := start(false)
	func() {
		defer stop()
		completed(1)
		if !sawBash.Load() || !sawAgent.Load() {
			t.Fatal("native request did not expose the Bash and subagent schemas")
		}
		// This native setting is SDK-only, even in the newer release. Do not
		// mistake a successful version upgrade for interactive TTL support.
		time.Sleep(400 * time.Millisecond)
		if err := syscall.Kill(inbox.PID, 0); err != nil {
			t.Fatal("interactive session unexpectedly exited", err)
		}
		deliver(inbox, "Follow up in this conversation")
		completed(2)
		if ttl != "" {
			var state map[string]any
			for ctx.Err() == nil {
				state = idleRequest("status", nil)
				if state["state"] == "completed" {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if state["state"] != "completed" || idleRequest("expire", state)["state"] != "draining" {
				t.Fatal("native idle expiry was not accepted", state)
			}
			for ctx.Err() == nil {
				data, _ := os.ReadFile(filepath.Join(root, "idle-stopped.json"))
				if strings.Contains(string(data), task.Session.ConversationID) {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("idle native owner exit was not confirmed")
		}
	}()
	task.Session.Generation++
	inbox, stop = start(true)
	defer stop()
	for ctx.Err() == nil {
		if _, err := os.Stat(inbox.Socket); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ttl != "" && idleRequest("status", nil)["state"] == "completed" {
		t.Fatal("resumed runtime adopted a previous generation's idle confirmation")
	}
	deliver(inbox, "Resume follow-up only")
	completed(3)
	if ttl != "" {
		var checkpoint map[string]any
		for ctx.Err() == nil {
			checkpoint = idleRequest("status", nil)
			if checkpoint["state"] == "completed" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		holdModel.Store(true)
		deliver(inbox, "Keep this model request active until the fixture releases it")
		select {
		case <-modelStarted:
		case <-ctx.Done():
			t.Fatal("native active-work fixture did not start")
		}
		if idleRequest("status", nil)["state"] != "unknown" || idleRequest("expire", checkpoint)["state"] != "unknown" {
			t.Fatal("new active work did not reject stale idle expiry")
		}
		if _, err := os.Stat(filepath.Join(root, "idle-drain.json")); !os.IsNotExist(err) {
			t.Fatal("active native work received shutdown intent", err)
		}
		close(modelRelease)
		completed(4)
	}
	approval.Store(true)
	deliver(inbox, "Exercise a native tool approval")
	ansi := regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")
	for ctx.Err() == nil {
		data, _ := os.ReadFile(terminalLog)
		text := ansi.ReplaceAllString(string(data), "")
		if strings.Contains(strings.Join(strings.Fields(text), ""), "Doyouwanttoproceed?") {
			if _, err := os.Stat(approvalMarker); !os.IsNotExist(err) {
				t.Fatal("native approval was bypassed", err)
			}
			if ttl != "" && idleRequest("status", nil)["state"] != "unknown" {
				t.Fatal("approval wait qualified as idle")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("native tool approval prompt was not displayed")
}
