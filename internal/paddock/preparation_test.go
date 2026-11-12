package paddock

import (
	"context"
	"testing"
	"time"
)

func TestPreparationRecoveryUsesLiveLease(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old := time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
	task := Task{ID: newID(), Status: "preparing", UpdatedAt: old, LastActivityAt: old, Session: Session{Backend: "screen", TerminalPath: "/bin/false", Name: "missing-task"}}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	release, err := store.taskLease(task.ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	current, _, err := service.Refresh(context.Background(), task)
	if err != nil || current.Status != "preparing" {
		t.Fatalf("live preparation misclassified: %s %v", current.Status, err)
	}
	release()
	current, _, err = service.Refresh(context.Background(), task)
	if err != nil || current.Status != "interrupted" || current.Error == "" {
		t.Fatalf("dead preparation not recovered: %+v %v", current, err)
	}
}
