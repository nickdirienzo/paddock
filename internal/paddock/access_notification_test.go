package paddock

import (
	"context"
	"testing"
)

func TestAccessNotificationDispositionPreservesAuthoritativeReceipt(t *testing.T) {
	for _, status := range []string{"absent", "pending", "claimed", "uncertain", "delivered"} {
		t.Run(status, func(t *testing.T) {
			s, task, _, requestID := accessReviewFixture(t)
			response := s.DecideAccessV1(context.Background(), requestID, accessReviewInput(t, s, task, requestID))
			if response.Error != nil || response.Receipt == nil {
				t.Fatal(response)
			}
			id := response.Receipt.ID
			if err := s.Store.updateAccessState(task.ID, func(state *accessTaskState) error {
				state.Decisions[id].Receipt.Notification.Status = "pending"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			state, err := s.Store.accessState(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			d := state.Decisions[id]
			// Another worker can claim after the first worker observed pending.
			other := openTestStore(t, s.Store.Home)
			if status != "absent" {
				r, claimed, err := other.ClaimMessage(task.ID, accessNotificationCaller, d.Receipt.Notification.ExternalID, d.NotificationBody)
				if err != nil || !claimed {
					t.Fatal(r, claimed, err)
				}
				if status != "claimed" {
					if _, err := other.FinishMessage(r, status, "", "fixture"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.blockAccessTarget(task, accessError("target_replaced", "Known replacement", false)); err != nil {
				t.Fatal(err)
			}
			s.disposeAccessNotification(task.ID, id, "target_replaced")
			got := s.accessDecisionResponse(task.ID, requestID, id)
			if got.Error != nil || got.Receipt == nil {
				t.Fatal(got)
			}
			if status == "absent" || status == "pending" {
				if got.Receipt.Notification.Status != "unavailable" {
					t.Fatal("definitely-unsent outbox was not disposed", got)
				}
				// A stale pending copy cannot create a claim after disposition,
				// even if the remote target is subsequently confirmed/restored.
				if err := s.Store.updateAccessState(task.ID, func(v *accessTaskState) error { v.TargetBlock = nil; return nil }); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := other.ClaimMessage(task.ID, accessNotificationCaller, d.Receipt.Notification.ExternalID, d.NotificationBody); err != nil || claimed {
					t.Fatal("stale worker reclaimed a disposed notification", claimed, err)
				}
			} else if got.Receipt.Notification.Status != status {
				t.Fatal("terminal disposition hid authoritative delivery state", status, got)
			}
		})
	}
}
