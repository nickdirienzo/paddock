package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// CodexLastTurn reads the saved native thread without starting a model turn.
// The TUI can remain running after a turn completes, so terminal state alone
// cannot tell an integration whether its latest request finished.
func CodexLastTurn(ctx context.Context, task Task) (AgentTurn, error) {
	unknown := AgentTurn{State: "unknown"}
	if task.RuntimeName() != "openshell" || task.Session.Agent != "codex" ||
		!conversationUUID.MatchString(task.Session.ConversationID) {
		return unknown, fmt.Errorf("task has no saved OpenShell Codex conversation")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := agentCommandContext(ctx, task, "app-server", "--stdio")
	input, err := command.StdinPipe()
	if err != nil {
		return unknown, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return unknown, err
	}
	if err = command.Start(); err != nil {
		return unknown, err
	}
	defer func() { _ = input.Close(); _ = command.Process.Kill(); _ = command.Wait() }()
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	call := func(id int, method string, params, result any) error {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for scanner.Scan() {
			var message struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				return err
			}
			if message.ID != id {
				continue
			}
			if len(message.Error) > 0 {
				return fmt.Errorf("Codex %s rejected the read", method)
			}
			if result == nil {
				return nil
			}
			return json.Unmarshal(message.Result, result)
		}
		return fmt.Errorf("Codex %s ended without a response (%v)", method, scanner.Err())
	}
	if err = call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "paddock", "version": "0.1.0"}}, nil); err != nil {
		return unknown, err
	}
	if err = encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return unknown, err
	}
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Cwd   string `json:"cwd"`
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err = call(2, "thread/read", map[string]any{"threadId": task.Session.ConversationID, "includeTurns": true}, &response); err != nil {
		return unknown, err
	}
	thread := response.Thread
	if thread.ID != task.Session.ConversationID || canonicalWorkspacePath(thread.Cwd) != canonicalWorkspacePath(task.Worktree) {
		return unknown, fmt.Errorf("saved Codex conversation does not match the task workspace")
	}
	if len(thread.Turns) == 0 {
		return AgentTurn{State: "none"}, nil
	}
	last := thread.Turns[len(thread.Turns)-1]
	switch last.Status {
	case "inProgress", "completed", "failed", "interrupted":
		return AgentTurn{ID: last.ID, State: last.Status}, nil
	default:
		return unknown, fmt.Errorf("Codex returned an unknown turn status")
	}
}
