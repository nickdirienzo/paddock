package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputCursorAndBoundedMessages(t *testing.T) {
	task := Task{ID: "task-one", Session: Session{ConversationID: newConversationID()}}
	events := append(publicMessage("msg", "turn", "final_answer", strings.Repeat("ø", outputChunkBytes)), finishTurn("turn", "turn", "completed"))
	if len(events) != 3 || events[0].Part != 1 || events[1].Part != 2 || len(events[0].Text) > outputChunkBytes {
		t.Fatalf("bad split: %+v", events)
	}
	page, err := pageOutput(task, events, "", 1)
	if err != nil || len(page.Events) != 1 || !page.HasMore {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = pageOutput(task, events, page.NextCursor, 1)
	if err != nil || page.Events[0].Part != 2 || !page.HasMore {
		t.Fatalf("second page: %+v %v", page, err)
	}
	page, err = pageOutput(task, events, page.NextCursor, 1)
	if err != nil || page.Events[0].Type != "turn_finished" || page.HasMore {
		t.Fatalf("completion page: %+v %v", page, err)
	}
	end, err := pageOutput(task, events, page.NextCursor, 1)
	if err != nil || len(end.Events) != 0 || end.NextCursor != page.NextCursor {
		t.Fatalf("end page: %+v %v", end, err)
	}
	other := task
	other.ID = "other"
	if _, err := pageOutput(other, events, page.NextCursor, 1); err == nil {
		t.Fatal("accepted another task's cursor")
	}
	if _, err := pageOutput(task, events[:2], page.NextCursor, 1); err == nil {
		t.Fatal("accepted missing cursor event")
	}
}

func TestClaudeOutputOnlyPublicTextAndTurnCompletion(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python is needed")
	}
	home, cwd, id := t.TempDir(), t.TempDir(), newConversationID()
	path := filepath.Join(home, "projects", "test", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(event map[string]any) {
		t.Helper()
		b, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.Write(append(b, '\n'))
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "uuid": "prompt-1", "message": map[string]any{"role": "user", "content": "private prompt"}})
	write(map[string]any{"type": "assistant", "sessionId": id, "cwd": cwd, "uuid": "msg-1", "message": map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []map[string]any{{"type": "thinking", "thinking": "secret"}, {"type": "text", "text": "Checking logs"}, {"type": "tool_use", "input": "secret command"}}}})
	write(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "uuid": "tool-result", "toolUseResult": map[string]any{"secret": true}, "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": "secret output"}}}})
	write(map[string]any{"type": "assistant", "sessionId": id, "cwd": cwd, "uuid": "sidechain", "isSidechain": true, "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "private sidechain"}}}})
	write(map[string]any{"type": "assistant", "sessionId": id, "cwd": cwd, "uuid": "msg-2", "message": map[string]any{"role": "assistant", "stop_reason": "end_turn", "content": []map[string]any{{"type": "text", "text": "Root cause found"}}}})
	write(map[string]any{"type": "system", "subtype": "turn_duration", "sessionId": id, "cwd": cwd, "uuid": "finish-1"})
	write(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "uuid": "prompt-2", "message": map[string]any{"role": "user", "content": "follow-up"}})
	write(map[string]any{"type": "assistant", "sessionId": id, "cwd": cwd, "uuid": "msg-3", "message": map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []map[string]any{{"type": "text", "text": "One more check"}}}})
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"assistant","message":`)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("python3", "-c", claudeOutputScript, home, id, cwd).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "secret") || strings.Contains(string(output), "private") {
		t.Fatalf("private content escaped: %s", output)
	}
	var events []OutputEvent
	if err := json.Unmarshal(output, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Text != "Checking logs" || events[1].Phase != "final_answer" || events[2].Type != "turn_finished" || events[2].TurnID != "prompt-1" || events[3].TurnID != "prompt-2" {
		t.Fatalf("wrong output: %+v", events)
	}
	if output, err := exec.Command("python3", "-c", claudeOutputScript, home, id, t.TempDir()).CombinedOutput(); err == nil {
		t.Fatalf("accepted wrong workspace: %s", output)
	}
}

func TestCodexOutputFiltersPrivateItemsAndFinishesPerTurn(t *testing.T) {
	cwd, id := t.TempDir(), newConversationID()
	thread, _ := json.Marshal(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": id, "cwd": cwd}}})
	turns := `{"id":3,"result":{"data":[{"id":"turn-1","status":"completed"},{"id":"turn-2","status":"inProgress"}],"nextCursor":null}}`
	items := `{"id":4,"result":{"data":[{"turnId":"turn-1","completedAtMs":1,"item":{"id":"private","type":"reasoning","text":"secret"}},{"turnId":"turn-1","completedAtMs":1,"item":{"id":"progress","type":"agentMessage","phase":"commentary","text":"Checking logs"}},{"turnId":"turn-1","completedAtMs":1,"item":{"id":"answer","type":"agentMessage","phase":"final_answer","text":"Cause found"}},{"turnId":"turn-2","item":{"id":"unfinished","type":"agentMessage","phase":"commentary","text":"draft"}},{"turnId":"turn-2","completedAtMs":2,"item":{"id":"new-progress","type":"agentMessage","phase":"commentary","text":"Looking again"}}],"nextCursor":null}}`
	client := script(t, `while IFS= read -r line; do
 case "$line" in
  *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
  *'"id":2'*) printf '%s\n' `+quote(string(thread))+` ;;
  *'"id":3'*) printf '%s\n' `+quote(turns)+` ;;
  *'"id":4'*) printf '%s\n' `+quote(items)+` ;;
 esac
done
`)
	task := Task{ID: "task", Runtime: "openshell", Worktree: cwd, Session: Session{Agent: "codex", AgentPath: "/usr/local/bin/codex", ConversationID: id}, OpenShell: &OpenShellTask{Name: "pd-test", Config: OpenShellConfig{Binary: client}}}
	events, err := codexOutput(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Text != "Checking logs" || events[1].Text != "Cause found" || events[2].Type != "turn_finished" || events[3].Text != "Looking again" {
		t.Fatalf("wrong output: %+v", events)
	}
	for _, event := range events {
		if strings.Contains(event.Text, "secret") || strings.Contains(event.Text, "draft") {
			t.Fatalf("private content escaped: %+v", event)
		}
	}
}
