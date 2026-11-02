package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Message uses the agent's native queue, never keyboard input into a terminal
// that might currently be displaying an approval dialog.
func (s Service) Message(ctx context.Context, task Task, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("message cannot be empty")
	}
	if task.Session.Agent != "codex" {
		return "", fmt.Errorf("native message delivery is not available for %s", task.Session.Agent)
	}
	task, observation, err := s.Refresh(ctx, task)
	if err != nil {
		return "", err
	}
	if task.Status != "running" || observation.State != "running" {
		return "", fmt.Errorf("task %s does not have a confirmed running agent", task.ID)
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
	command := exec.CommandContext(ctx, task.Session.AgentPath, "queue", "--thread", thread, "--message", body)
	command.Env = agentControlEnv(task)
	if _, err = command.Output(); err != nil {
		return thread, fmt.Errorf("Codex queue delivery outcome uncertain: %w; inspect the session before retrying", err)
	}
	return thread, nil
}

func agentControlEnv(task Task) []string {
	env := credentialFreeEnvironment(os.Environ())
	if task.Session.AgentHome != "" {
		filtered := make([]string, 0, len(env)+1)
		for _, v := range env {
			if !strings.HasPrefix(v, "CODEX_HOME=") {
				filtered = append(filtered, v)
			}
		}
		env = append(filtered, "CODEX_HOME="+task.Session.AgentHome)
	}
	return env
}

// Read persisted session metadata through Codex's public JSON-RPC protocol.
// This short-lived process never starts a thread or executes hooks/model turns.
func codexTaskThread(ctx context.Context, task Task) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, task.Session.AgentPath, "app-server", "--stdio")
	command.Env = agentControlEnv(task)
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
	cwd, err := filepath.EvalSymlinks(task.Worktree)
	if err != nil {
		return "", err
	}
	var response struct {
		Data []struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	if err = call(2, "thread/list", map[string]any{"cwd": cwd, "limit": 100, "archived": false, "sourceKinds": []string{"cli"}, "modelProviders": []string{}}, &response); err != nil {
		return "", err
	}
	if response.NextCursor != nil {
		return "", fmt.Errorf("too many Codex sessions for task worktree; cannot route unambiguously")
	}
	var matches []string
	for _, thread := range response.Data {
		path, e := filepath.EvalSymlinks(thread.Cwd)
		if e == nil && path == cwd && (task.Session.ConversationID == "" || thread.ID == task.Session.ConversationID) {
			matches = append(matches, thread.ID)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one Codex session for task worktree; found %d", len(matches))
	}
	return matches[0], nil
}
