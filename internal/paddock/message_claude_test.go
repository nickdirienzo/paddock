package paddock

import (
	"context"
	"errors"
	"testing"
)

func TestClaudeInboxDeliveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		output             string
		offline, uncertain bool
	}{
		{"queued", false, false}, {"offline", true, false}, {"uncertain", false, true}, {"unexpected", false, true},
	} {
		t.Run(tc.output, func(t *testing.T) {
			store := openTestStore(t, t.TempDir())
			task := Task{ID: "012345678901234567890123", Status: "running"}
			err := store.Insert(task)
			if err != nil {
				t.Fatal(err)
			}
			task.Runtime = "openshell"
			task.Session = Session{Agent: "claude", ConversationID: "saved", Generation: 2}
			task.OpenShell = &OpenShellTask{Name: "test", Config: OpenShellConfig{Binary: script(t, "cat >/dev/null\necho "+tc.output), Gateway: "test", Workspace: "default"}}
			id, err := (Service{Store: store}).messageClaude(context.Background(), task, "hello")
			if errors.Is(err, ErrAgentOffline) != tc.offline || (err != nil && !errors.Is(err, ErrAgentOffline)) != tc.uncertain {
				t.Fatalf("id=%q err=%v", id, err)
			}
			if err == nil && id != "saved" {
				t.Fatal("lost conversation")
			}
		})
	}
}
