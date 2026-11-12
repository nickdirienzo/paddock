package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestClaudeLastTurnFromNativeTranscript(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python is needed for the OpenShell Claude transcript reader")
	}
	home, cwd, id := t.TempDir(), t.TempDir(), newConversationID()
	path := filepath.Join(home, "projects", "test", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	read := func(want string) {
		t.Helper()
		output, err := exec.Command("python3", "-c", claudeTurnScript, home, id, cwd).Output()
		if err != nil {
			t.Fatalf("Claude transcript read failed: %v", err)
		}
		var turn AgentTurn
		if err := json.Unmarshal(output, &turn); err != nil || turn.State != want {
			t.Fatalf("turn = %+v, parse error = %v; want %s", turn, err, want)
		}
	}
	appendEvent := func(event map[string]any) {
		t.Helper()
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.Write(append(data, '\n'))
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	read("none")
	appendEvent(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "uuid": "first", "message": map[string]any{"role": "user", "content": "investigate"}})
	read("inProgress")
	appendEvent(map[string]any{"type": "assistant", "sessionId": id, "cwd": filepath.Join(cwd, "subdir"), "message": map[string]any{"role": "assistant", "stop_reason": "tool_use"}})
	appendEvent(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "toolUseResult": map[string]any{"ok": true}, "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result"}}}})
	appendEvent(map[string]any{"type": "system", "subtype": "turn_duration", "sessionId": id, "cwd": cwd})
	read("completed")
	appendEvent(map[string]any{"type": "queue-operation", "operation": "enqueue", "sessionId": id})
	read("inProgress")
	appendEvent(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "uuid": "followup", "message": map[string]any{"role": "user", "content": "follow up"}})
	appendEvent(map[string]any{"type": "system", "subtype": "turn_duration", "sessionId": id, "cwd": cwd})
	read("completed")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"user","message":`)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	read("completed") // An incomplete append must not start a phantom turn.
	if output, err := exec.Command("python3", "-c", claudeTurnScript, home, id, t.TempDir()).CombinedOutput(); err == nil {
		t.Fatalf("accepted a transcript from another workspace: %s", output)
	}
}

func TestCodexLastTurnReadsSavedOpenShellConversation(t *testing.T) {
	cwd, id := t.TempDir(), newConversationID()
	for _, tc := range []struct {
		name, status, cwd, want string
		wantError               bool
	}{
		{name: "completed", status: "completed", cwd: cwd, want: "completed"},
		{name: "active", status: "inProgress", cwd: cwd, want: "inProgress"},
		{name: "unknown status", status: "unexpected", cwd: cwd, want: "unknown", wantError: true},
		{name: "wrong workspace", status: "completed", cwd: "/another/worktree", want: "unknown", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := json.Marshal(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{
				"id": id, "cwd": tc.cwd, "turns": []map[string]string{{"id": "first", "status": "completed"}, {"id": "latest", "status": tc.status}},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			client := script(t, `while IFS= read -r line; do
 case "$line" in
  *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
  *'"id":2'*) printf '%s\n' `+quote(string(response))+` ;;
 esac
done
`)
			task := Task{Runtime: "openshell", Worktree: cwd, Session: Session{Agent: "codex", AgentPath: "/usr/local/bin/codex", ConversationID: id}, OpenShell: &OpenShellTask{Name: "pd-test", Config: OpenShellConfig{Binary: client}}}
			turn, err := CodexLastTurn(context.Background(), task)
			if (err != nil) != tc.wantError || turn.State != tc.want {
				t.Fatalf("turn = %+v, err = %v; want state %q, error %t", turn, err, tc.want, tc.wantError)
			}
			if err == nil && turn.ID != "latest" {
				t.Fatalf("wrong last turn: %+v", turn)
			}
		})
	}
}
