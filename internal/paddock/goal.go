package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func parseGoalPrompt(prompt string) (string, bool, error) {
	text := strings.TrimSpace(prompt)
	if !strings.HasPrefix(text, "/goal") {
		return "", false, nil
	}
	if next, _ := utf8.DecodeRuneInString(text[5:]); len(text) > 5 && !unicode.IsSpace(next) {
		return "", false, nil
	}
	objective := strings.TrimSpace(text[5:])
	switch objective {
	case "", "pause", "resume", "clear":
		return "", true, fmt.Errorf("a new goal needs an objective: /goal <what should be accomplished>")
	}
	return objective, true, nil
}

// Codex limits the durable objective to 4000 Unicode characters. Keep a long
// specification intact in model-visible history and use its first line as the
// durable objective, with an explicit reference to all of the original criteria.
func nativeGoalObjective(objective string) string {
	if utf8.RuneCountInString(objective) <= 4000 {
		return objective
	}
	const reference = "\n\nComplete the full goal specification recorded in this conversation, including all acceptance criteria, scope limits, and constraints."
	title := strings.SplitN(objective, "\n", 2)[0]
	if chars := []rune(title); len(chars) > 4000-utf8.RuneCountInString(reference) {
		title = string(chars[:4000-utf8.RuneCountInString(reference)-1]) + "…"
	}
	return title + reference
}

// goalRPC owns a temporary metadata server, never the interactive agent's owner.
// Preparation creates only a paused goal. Activation uses a second server that
// does not load the thread, so model work starts only when the TUI resumes it.
type goalRPC struct {
	command *exec.Cmd
	input   io.WriteCloser
	encoder *json.Encoder
	scanner *bufio.Scanner
	nextID  int
}

type goalRPCError struct {
	method  string
	message string
}

func (e *goalRPCError) Error() string {
	if e.message == "goals feature is disabled" {
		return "Codex Goals are disabled; enable Goals before starting a /goal task"
	}
	return fmt.Sprintf("Codex rejected %s; native goal setup was not confirmed", e.method)
}

func openGoalRPC(ctx context.Context, task Task, home, exe string, overrides ...string) (*goalRPC, error) {
	args := codexTrustArgs(task, overrides...)
	args = append(args, "app-server", "--stdio")
	command := agentCommandContext(ctx, task, args...)
	if task.ReadOnly() {
		// Session-start hooks and project config must share the agent's guard.
		command = sandboxExec(ctx, task, false, append([]string{"/usr/bin/python3", investigationGuardDir + "/exec.py", task.Session.AgentPath}, args...)...)
	}
	// Resolve repository-scoped config exactly as an interactive launch would.
	if task.RuntimeName() == "host" {
		command.Dir = task.Worktree
		command.Env = agentLaunchEnv(task, home, exe)
	}
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	if err = command.Start(); err != nil {
		input.Close()
		return nil, err
	}
	rpc := &goalRPC{command: command, input: input, encoder: json.NewEncoder(input), scanner: bufio.NewScanner(output)}
	rpc.scanner.Buffer(make([]byte, 4096), 8<<20)
	if err = rpc.call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "paddock", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, nil); err == nil {
		err = rpc.encoder.Encode(map[string]any{"method": "initialized"})
	}
	if err != nil {
		rpc.close()
		return nil, err
	}
	return rpc, nil
}

func (r *goalRPC) close() {
	_ = r.input.Close()
	// These servers have no admitted model turns. Do not stop a shared daemon.
	_ = r.command.Process.Kill()
	_ = r.command.Wait()
}

func (r *goalRPC) call(method string, params, result any) error {
	r.nextID++
	if err := r.encoder.Encode(map[string]any{"id": r.nextID, "method": method, "params": params}); err != nil {
		return err
	}
	for r.scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(r.scanner.Bytes(), &message); err != nil {
			return fmt.Errorf("invalid Codex %s response", method)
		}
		if message.Method != "" && len(message.ID) != 0 {
			return fmt.Errorf("Codex goal setup requires interactive input; complete agent setup before starting a goal")
		}
		if string(message.ID) != fmt.Sprint(r.nextID) {
			continue
		}
		if len(message.Error) != 0 {
			var remote struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(message.Error, &remote)
			return &goalRPCError{method: method, message: remote.Message}
		}
		if result != nil {
			return json.Unmarshal(message.Result, result)
		}
		return nil
	}
	return fmt.Errorf("Codex %s ended without confirmation (%v)", method, r.scanner.Err())
}

type nativeGoal struct {
	ThreadID  string `json:"threadId"`
	Objective string `json:"objective"`
	Status    string `json:"status"`
}

func prepareInitialGoal(ctx context.Context, store *Store, task *Task, exe string, overrides ...string) error {
	objective, goal, err := parseGoalPrompt(task.Prompt)
	if err != nil || !goal || task.Session.GoalInitialized {
		return err
	}
	if task.Session.Agent != "codex" {
		return fmt.Errorf("/goal requires the Codex agent")
	}
	objective = nativeGoalObjective(objective)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if task.Session.ConversationID == "" {
		if err = preparePausedGoal(ctx, store, task, exe, objective, overrides...); err != nil {
			return err
		}
	}
	// Do not resume here: activating an unloaded thread saves the native goal
	// without starting model work in the temporary metadata server.
	rpc, err := openGoalRPC(ctx, *task, store.Home, exe, overrides...)
	if err != nil {
		return err
	}
	defer rpc.close()
	var read struct {
		Thread struct{ ID, Cwd string } `json:"thread"`
	}
	if err = rpc.call("thread/read", map[string]any{"threadId": task.Session.ConversationID, "includeTurns": false}, &read); err != nil {
		return err
	}
	if read.Thread.ID != task.Session.ConversationID || canonicalWorkspacePath(read.Thread.Cwd) != canonicalWorkspacePath(task.Worktree) {
		return fmt.Errorf("saved goal conversation does not match the task workspace")
	}
	var response struct {
		Goal *nativeGoal `json:"goal"`
	}
	if err = rpc.call("thread/goal/get", map[string]any{"threadId": task.Session.ConversationID}, &response); err != nil {
		return err
	}
	if response.Goal == nil || response.Goal.Objective != objective || response.Goal.ThreadID != task.Session.ConversationID {
		return fmt.Errorf("saved native goal does not match the requested objective")
	}
	if response.Goal.Status == "paused" {
		if err = rpc.call("thread/goal/set", map[string]any{"threadId": task.Session.ConversationID, "status": "active", "origin": "user"}, &response); err != nil {
			return err
		}
	}
	if response.Goal == nil || response.Goal.Status != "active" || response.Goal.Objective != objective || response.Goal.ThreadID != task.Session.ConversationID {
		return fmt.Errorf("native goal activation was not confirmed")
	}
	if err = store.UpdateSession(task.ID, func(session *Session) { session.GoalInitialized = true }); err != nil {
		return err
	}
	task.Session.GoalInitialized = true
	return nil
}

func preparePausedGoal(ctx context.Context, store *Store, task *Task, exe, objective string, overrides ...string) error {
	rpc, err := openGoalRPC(ctx, *task, store.Home, exe, overrides...)
	if err != nil {
		return err
	}
	defer rpc.close()
	params := map[string]any{"cwd": task.Worktree}
	if task.RuntimeName() == "openshell" {
		params["sandbox"], params["approvalPolicy"] = "danger-full-access", "on-request"
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = rpc.call("thread/start", params, &started); err != nil {
		return err
	}
	thread := started.Thread.ID
	if !conversationUUID.MatchString(thread) {
		return fmt.Errorf("Codex returned an invalid goal conversation ID")
	}
	if err = rpc.call("thread/goal/set", map[string]any{"threadId": thread, "objective": objective, "status": "paused", "origin": "user"}, nil); err != nil {
		return err
	}
	contextTask := *task
	contextTask.Prompt = ""
	context := strings.TrimSpace(agentPrompt(contextTask, store.Home, exe))
	fullObjective, _, _ := parseGoalPrompt(task.Prompt)
	if objective != fullObjective {
		context = "Full goal specification:\n\n" + fullObjective + "\n\n" + context
	}
	if err = rpc.call("thread/inject_items", map[string]any{"threadId": thread, "items": []any{map[string]any{
		"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": context}},
	}}}, nil); err != nil {
		return err
	}
	if err = store.UpdateSession(task.ID, func(session *Session) { session.ConversationID = thread }); err != nil {
		return err
	}
	task.Session.ConversationID = thread
	return nil
}
