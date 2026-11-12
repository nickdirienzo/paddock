package paddock

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInvestigationAgentStartsThroughReadOnlyGuard(t *testing.T) {
	task := Task{ID: newID(), Runtime: "openshell", Worktree: "/sandbox/worktree", Provenance: TaskProvenance{Profile: "investigation"},
		Session:   Session{Agent: "codex", AgentPath: "/usr/local/bin/codex", Generation: 1},
		OpenShell: &OpenShellTask{Name: "pd-test", ReadOnlyGuard: true, ReadOnlyGuardVersion: investigationGuardVersion, Config: OpenShellConfig{Binary: "/usr/bin/openshell", Gateway: "test", Workspace: "default"}}}
	command := AgentCommand(task, t.TempDir(), "/usr/bin/paddock")
	args := strings.Join(command.Args, " ")
	if !strings.Contains(args, investigationGuardDir+"/launch.py") || !strings.Contains(args, investigationGuardDir+"/codex") || strings.Contains(args, "/usr/local/bin/paddock-agent 1 /usr/local/bin/codex") {
		t.Fatalf("investigation bypasses guard: %s", args)
	}
}

func TestLegacyInvestigationGuardCannotResumeOrReceiveMessages(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := Task{ID: newID(), Runtime: "openshell", Status: "stopped", Provenance: TaskProvenance{Profile: "investigation"},
		Session: Session{Agent: "codex", ConversationID: newConversationID()}, OpenShell: &OpenShellTask{Name: "pd-legacy", Ready: true, ReadOnlyGuard: true, ReadOnlyGuardVersion: 1}}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	if _, err := service.resumeLocked(context.Background(), task.ID); err == nil || !strings.Contains(err.Error(), "read-only guard") {
		t.Fatalf("outdated investigation guard resumed: %v", err)
	}
	if _, err := service.Message(context.Background(), task, "continue"); !errors.Is(err, ErrAgentOffline) || !strings.Contains(err.Error(), "read-only guard") {
		t.Fatalf("outdated investigation guard received message: %v", err)
	}
}
