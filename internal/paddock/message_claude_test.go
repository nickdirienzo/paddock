package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestOpenShellClaudeReviewDelivery(t *testing.T) {
	g, remote, store, watch, _ := githubFixture(t)
	log, args := filepath.Join(t.TempDir(), "messages"), filepath.Join(t.TempDir(), "arguments")
	task, _ := store.Get(watch.TaskID)
	task.Runtime = "openshell"
	task.Session.Agent, task.Session.ConversationID, task.Session.Generation = "claude", newConversationID(), 3
	task.OpenShell = &OpenShellTask{Name: "test", Config: OpenShellConfig{Gateway: "test", Workspace: "default", Binary: script(t,
		"printf '%s\\n' \"$@\" > "+quote(args)+"\ncat >> "+quote(log)+"\nprintf '\\n' >> "+quote(log)+"\necho queued")}}
	if err := store.update(task.ID, func(current *Task) error { *current = task; return nil }); err != nil {
		t.Fatal(err)
	}
	summary, members := reviewFixture()
	remote.comments = append(members, summary)
	for range 2 {
		if _, err := g.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(log)
	var payload struct{ Message string }
	if err != nil || json.Unmarshal(data, &payload) != nil {
		t.Fatalf("missing or duplicate review delivery: %s %v", data, err)
	}
	for _, body := range []string{summary.Body, members[0].Body, members[1].Body} {
		if !strings.Contains(payload.Message, body) {
			t.Fatal("lost review context", body)
		}
	}
	data, err = os.ReadFile(args)
	if err != nil || !strings.HasSuffix(string(data), "/usr/local/bin/paddock-claude-message\n"+task.Session.ConversationID+"\n3\n") || remote.reactionCalls != 2 {
		t.Fatalf("wrong inbox identity or receipts: %s %v reactions=%d", data, err, remote.reactionCalls)
	}
}
