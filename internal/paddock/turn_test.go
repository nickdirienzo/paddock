package paddock

import (
	"context"
	"encoding/json"
	"testing"
)

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
