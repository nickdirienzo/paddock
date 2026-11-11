package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrAgentOffline means delivery was not attempted and is safe to retry later.
var ErrAgentOffline = errors.New("agent is offline")

// Message uses the agent's native queue, never keyboard input into a terminal
// that might currently be displaying an approval dialog.
func (s Service) Message(ctx context.Context, task Task, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("message cannot be empty")
	}
	if !nativeMessageSupported(task) {
		return "", fmt.Errorf("native message delivery is not available for %s", task.Session.Agent)
	}
	release, err := s.Store.taskLease(task.ID, "operation")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrAgentOffline, err)
	}
	defer release()
	task, err = s.Store.Get(task.ID)
	if err != nil {
		return "", err
	}
	task, observation, err := s.Refresh(ctx, task)
	if err != nil {
		return "", err
	}
	if task.Status != "running" || observation.State != "running" {
		if task.Session.ConversationID == "" {
			return "", fmt.Errorf("%w: task %s has no saved conversation", ErrAgentOffline, task.ID)
		}
		task, err = s.resumeLocked(ctx, task.ID)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrAgentOffline, err)
		}
		_, observation = terminalFor(task.Session).Inspect(ctx, task)
		if task.Status != "running" || observation.State != "running" {
			return "", fmt.Errorf("%w: agent has not started yet", ErrAgentOffline)
		}
	}
	if task.Session.Agent == "claude" {
		return s.messageClaude(ctx, task, body)
	}
	thread, err := codexTaskThread(ctx, task)
	if err != nil {
		return "", err
	}
	if err = s.Store.UpdateSession(task.ID, func(v *Session) { v.ConversationID = thread }); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if task.RuntimeName() == "openshell" {
		if _, err = sandboxOutput(ctx, task, "/usr/local/bin/paddock-codex-state", "owner", thread); err != nil {
			return "", fmt.Errorf("%w: native owner not ready", ErrAgentOffline)
		}
	}
	queueArgs := []string{"queue", "--thread", thread, "--message", body}
	if task.RuntimeName() == "openshell" {
		queueArgs = append(queueArgs, "--remote", "unix:///home/sandbox/.codex/app-server-control/app-server-control.sock")
	}
	command := agentCommandContext(ctx, task, queueArgs...)
	if err = s.Store.TouchActivity(task.ID); err != nil {
		return "", err
	}
	if _, err = command.Output(); err != nil {
		return thread, fmt.Errorf("Codex queue delivery outcome uncertain: %w; inspect the session before retrying", err)
	}
	if task.RuntimeName() == "openshell" {
		if _, err = sandboxOutput(ctx, task, "/usr/local/bin/paddock-codex-state", "start-queued", thread); err != nil {
			return thread, fmt.Errorf("message queued, but native turn start is unconfirmed; inspect before retrying: %w", err)
		}
	}
	return thread, nil
}

func agentControlEnv(task Task) []string {
	env := credentialFreeEnvironment(os.Environ())
	if task.Session.AgentHome != "" {
		key := "CODEX_HOME"
		if task.Session.Agent == "claude" {
			key = "CLAUDE_CONFIG_DIR"
		}
		filtered := make([]string, 0, len(env)+1)
		for _, v := range env {
			if !strings.HasPrefix(v, key+"=") {
				filtered = append(filtered, v)
			}
		}
		env = append(filtered, key+"="+task.Session.AgentHome)
	}
	return env
}

// Read persisted session metadata through Codex's public JSON-RPC protocol.
// This short-lived process never starts a thread or executes hooks/model turns.
func codexTaskThread(ctx context.Context, task Task) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := agentCommandContext(ctx, task, "app-server", "--stdio")
	input, err := command.StdinPipe()
	if err != nil {
		return "", err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err = command.Start(); err != nil {
		return "", err
	}
	defer func() { input.Close(); command.Process.Kill(); command.Wait() }()
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	call := func(id int, method string, params, result any) error {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for scanner.Scan() {
			var msg struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				return err
			}
			if msg.ID != id {
				continue
			}
			if len(msg.Error) > 0 {
				return fmt.Errorf("Codex %s failed: %s", method, msg.Error)
			}
			if result == nil {
				return nil
			}
			return json.Unmarshal(msg.Result, result)
		}
		return fmt.Errorf("Codex %s ended without a response (%v)", method, scanner.Err())
	}
	if err = call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "paddock", "version": "0.1.0"}}, nil); err != nil {
		return "", err
	}
	if err = encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return "", err
	}
	cwd := canonicalWorkspacePath(task.Worktree)
	if task.Session.ConversationID != "" {
		// A persisted ID is authoritative. Native resumes can change sourceKind;
		// do not lose an existing thread through the CLI-only discovery filter.
		var response struct {
			Thread struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"thread"`
		}
		if err = call(2, "thread/read", map[string]any{"threadId": task.Session.ConversationID, "includeTurns": false}, &response); err != nil {
			return "", err
		}
		if response.Thread.ID != task.Session.ConversationID || canonicalWorkspacePath(response.Thread.Cwd) != cwd {
			return "", fmt.Errorf("saved Codex conversation does not match the task workspace")
		}
		return response.Thread.ID, nil
	}
	var response struct {
		Data []struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	// Codex TUI sessions may be recorded as vscode even when launched by the
	// host CLI. Keep subagent sessions excluded while finding the task thread.
	sources := []string{"cli", "appServer", "vscode"}
	if err = call(2, "thread/list", map[string]any{"cwd": cwd, "limit": 100, "archived": false, "sourceKinds": sources, "modelProviders": []string{}}, &response); err != nil {
		return "", err
	}
	if response.NextCursor != nil {
		return "", fmt.Errorf("too many Codex sessions for task worktree; cannot route unambiguously")
	}
	var matches []string
	for _, thread := range response.Data {
		path := canonicalWorkspacePath(thread.Cwd)
		if path == cwd && (task.Session.ConversationID == "" || thread.ID == task.Session.ConversationID) {
			matches = append(matches, thread.ID)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one Codex session for task worktree; found %d", len(matches))
	}
	return matches[0], nil
}
