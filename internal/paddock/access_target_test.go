package paddock

import (
	"context"
	"errors"
	"testing"
)

func TestAccessKnownTargetLossBlocksPinnedNotification(t *testing.T) {
	s, task, _, requestID := accessReviewFixture(t)
	if err := s.Store.update(task.ID, func(current *Task) error { current.Session.Agent = "codex"; return nil }); err != nil {
		t.Fatal(err)
	}
	task.Session.Agent = "codex"
	s.messageDelivery = func(context.Context, Task, string) (string, error) { return "", ErrAgentOffline }
	response := s.DecideAccessV1(context.Background(), requestID, accessReviewInput(t, s, task, requestID))
	if response.Error != nil || response.Receipt == nil {
		t.Fatal(response)
	}
	if err := s.blockAccessTarget(task, accessError("target_replaced", "Known remote replacement", false)); err != nil {
		t.Fatal(err)
	}
	// The registry still carries the old ready identity. The pinned native send
	// must honor later authoritative loss evidence before touching a real agent.
	thread, err := s.messageExpected(context.Background(), task, "access result", &task)
	if !errors.Is(err, ErrMessageTargetChanged) || thread != "" {
		t.Fatal(thread, err)
	}
}
