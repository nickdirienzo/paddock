package paddock

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// OutputEvent is public agent text or a native turn's terminal state. IDs are
// stable within the pinned conversation; text is split at rune boundaries to
// keep each event bounded without losing a long answer.
type OutputEvent struct {
	ID     string `json:"id"`
	Type   string `json:"type"` // message, turn_finished
	TurnID string `json:"turn_id"`
	Phase  string `json:"phase,omitempty"` // commentary, partial_answer, final_answer
	Text   string `json:"text,omitempty"`
	Part   int    `json:"part,omitempty"`
	Parts  int    `json:"parts,omitempty"`
	State  string `json:"state,omitempty"` // completed, failed, interrupted
}

type OutputPage struct {
	TaskID     string        `json:"task_id"`
	Events     []OutputEvent `json:"events"`
	NextCursor string        `json:"next_cursor,omitempty"`
	HasMore    bool          `json:"has_more"`
}

type outputCursor struct {
	Task         string `json:"task"`
	Conversation string `json:"conversation"`
	Event        string `json:"event"`
}

const outputChunkBytes = 64 << 10

func publicMessage(id, turnID, phase, message string) []OutputEvent {
	if strings.TrimSpace(message) == "" {
		return nil
	}
	chunks := []string{}
	for len(message) > 0 {
		n := len(message)
		if n > outputChunkBytes {
			n = outputChunkBytes
			for n > 0 && n < len(message) && (message[n]&0xc0) == 0x80 {
				n--
			}
		}
		chunks = append(chunks, message[:n])
		message = message[n:]
	}
	events := make([]OutputEvent, 0, len(chunks))
	for i, chunk := range chunks {
		e := OutputEvent{ID: fmt.Sprintf("m:%s:%d", id, i), Type: "message", TurnID: turnID, Phase: phase, Text: chunk}
		if len(chunks) > 1 {
			e.Part, e.Parts = i+1, len(chunks)
		}
		events = append(events, e)
	}
	return events
}

func finishTurn(id, turnID, state string) OutputEvent {
	return OutputEvent{ID: "t:" + id, Type: "turn_finished", TurnID: turnID, State: state}
}

func pageOutput(task Task, events []OutputEvent, after string, limit int) (OutputPage, error) {
	page := OutputPage{TaskID: task.ID, Events: []OutputEvent{}}
	if limit < 1 || limit > 100 {
		return page, fmt.Errorf("output limit must be between 1 and 100")
	}
	start := 0
	if after != "" {
		data, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil {
			return page, fmt.Errorf("invalid output cursor")
		}
		var cursor outputCursor
		if json.Unmarshal(data, &cursor) != nil || cursor.Task != task.ID || cursor.Conversation != task.Session.ConversationID || cursor.Event == "" {
			return page, fmt.Errorf("output cursor does not match this task conversation")
		}
		found := false
		for i, e := range events {
			if e.ID == cursor.Event {
				start, found = i+1, true
				break
			}
		}
		if !found {
			return page, fmt.Errorf("output cursor event is no longer available")
		}
	}
	end := start + limit
	if end > len(events) {
		end = len(events)
	}
	page.Events = append(page.Events, events[start:end]...)
	page.HasMore = end < len(events)
	if len(page.Events) > 0 {
		c, _ := json.Marshal(outputCursor{Task: task.ID, Conversation: task.Session.ConversationID, Event: page.Events[len(page.Events)-1].ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(c)
	} else {
		page.NextCursor = after
	}
	return page, nil
}

// NativeOutput reads persisted native history. It never resumes a thread or
// starts a model turn, and never returns prompts, reasoning, tools, or terminal
// output. A missing history or malformed native response is an error.
func NativeOutput(ctx context.Context, task Task, after string, limit int) (OutputPage, error) {
	if limit < 1 || limit > 100 {
		return OutputPage{}, fmt.Errorf("output limit must be between 1 and 100")
	}
	if task.RuntimeName() != "openshell" || !conversationUUID.MatchString(task.Session.ConversationID) || task.OpenShell == nil {
		return OutputPage{}, fmt.Errorf("task has no saved OpenShell conversation")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var events []OutputEvent
	var err error
	switch task.Session.Agent {
	case "codex":
		events, err = codexOutput(ctx, task)
	case "claude":
		events, err = claudeOutput(ctx, task)
	default:
		return OutputPage{}, fmt.Errorf("unsupported native agent %q", task.Session.Agent)
	}
	if err != nil {
		return OutputPage{}, err
	}
	return pageOutput(task, events, after, limit)
}

//go:embed output_claude.py
var claudeOutputScript string

func claudeOutput(ctx context.Context, task Task) ([]OutputEvent, error) {
	if task.Session.AgentHome == "" {
		return nil, fmt.Errorf("task has no saved Claude agent home")
	}
	output, err := sandboxOutput(ctx, task, "python3", "-c", claudeOutputScript,
		task.Session.AgentHome, task.Session.ConversationID, task.Worktree)
	if err != nil {
		return nil, err
	}
	var native []struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		TurnID string `json:"turn_id"`
		Phase  string `json:"phase"`
		Text   string `json:"text"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(output), &native); err != nil {
		return nil, fmt.Errorf("invalid Claude output response: %w", err)
	}
	events := []OutputEvent{}
	for _, e := range native {
		if e.ID == "" || e.TurnID == "" {
			return nil, fmt.Errorf("Claude output has no stable native ID")
		}
		switch e.Type {
		case "message":
			if e.Phase != "commentary" && e.Phase != "final_answer" {
				return nil, fmt.Errorf("Claude output has an unknown message phase")
			}
			events = append(events, publicMessage(e.ID, e.TurnID, e.Phase, e.Text)...)
		case "turn_finished":
			if e.State != "completed" {
				return nil, fmt.Errorf("Claude output has an unknown terminal state")
			}
			events = append(events, finishTurn(e.ID, e.TurnID, e.State))
		default:
			return nil, fmt.Errorf("Claude output has an unknown event type")
		}
	}
	return events, nil
}

type codexList[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"nextCursor"`
}

func codexOutput(ctx context.Context, task Task) ([]OutputEvent, error) {
	command := agentCommandContext(ctx, task, "app-server", "--stdio")
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = command.Start(); err != nil {
		return nil, err
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
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
				return err
			}
			if response.ID != id {
				continue
			}
			if len(response.Error) > 0 && string(response.Error) != "null" {
				return fmt.Errorf("Codex %s rejected the read", method)
			}
			if result == nil {
				return nil
			}
			return json.Unmarshal(response.Result, result)
		}
		return fmt.Errorf("Codex %s ended without a response (%v)", method, scanner.Err())
	}
	if err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "paddock", "version": "0.1.0"}}, nil); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return nil, err
	}
	var thread struct {
		Thread struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"thread"`
	}
	if err := call(2, "thread/read", map[string]any{"threadId": task.Session.ConversationID}, &thread); err != nil {
		return nil, err
	}
	if thread.Thread.ID != task.Session.ConversationID || canonicalWorkspacePath(thread.Thread.Cwd) != canonicalWorkspacePath(task.Worktree) {
		return nil, fmt.Errorf("saved Codex conversation does not match the task workspace")
	}
	type turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	turns := []turn{}
	requestID := 3
	cursor := ""
	for {
		params := map[string]any{"threadId": task.Session.ConversationID, "sortDirection": "asc", "itemsView": "notLoaded", "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page codexList[turn]
		if err := call(requestID, "thread/turns/list", params, &page); err != nil {
			return nil, err
		}
		requestID++
		turns = append(turns, page.Data...)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("Codex turn pagination did not advance")
		}
		cursor = page.NextCursor
	}
	type item struct {
		TurnID        string `json:"turnId"`
		CompletedAtMs *int64 `json:"completedAtMs"`
		Item          struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Phase string `json:"phase"`
			Text  string `json:"text"`
		} `json:"item"`
	}
	items := []item{}
	cursor = ""
	for {
		params := map[string]any{"threadId": task.Session.ConversationID, "sortDirection": "asc", "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page codexList[item]
		if err := call(requestID, "thread/items/list", params, &page); err != nil {
			return nil, err
		}
		requestID++
		items = append(items, page.Data...)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("Codex item pagination did not advance")
		}
		cursor = page.NextCursor
	}
	byTurn := map[string][]OutputEvent{}
	for _, i := range items {
		if i.Item.Type != "agentMessage" || i.CompletedAtMs == nil {
			continue
		}
		if i.Item.Phase != "commentary" && i.Item.Phase != "partial_answer" && i.Item.Phase != "final_answer" {
			continue
		}
		if i.TurnID == "" || i.Item.ID == "" {
			return nil, fmt.Errorf("Codex public message has no stable native ID")
		}
		byTurn[i.TurnID] = append(byTurn[i.TurnID], publicMessage(i.Item.ID, i.TurnID, i.Item.Phase, i.Item.Text)...)
	}
	events := []OutputEvent{}
	for _, turn := range turns {
		if turn.ID == "" {
			return nil, fmt.Errorf("Codex turn has no stable native ID")
		}
		events = append(events, byTurn[turn.ID]...)
		delete(byTurn, turn.ID)
		switch turn.Status {
		case "completed", "failed", "interrupted":
			events = append(events, finishTurn(turn.ID, turn.ID, turn.Status))
		case "inProgress":
		default:
			return nil, fmt.Errorf("Codex returned an unknown turn status")
		}
	}
	if len(byTurn) > 0 {
		return nil, fmt.Errorf("Codex public message has no matching turn")
	}
	return events, nil
}
