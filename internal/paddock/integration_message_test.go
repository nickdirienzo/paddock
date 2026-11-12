package paddock

import "testing"

func TestMessageIdempotencyAndUncertainDelivery(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := Task{ID: newID()}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	first, claimed, err := store.ClaimMessage(task.ID, "100:service", "event-1", "follow up")
	if err != nil || !claimed || first.Status != "claimed" {
		t.Fatalf("claim: %+v %t %v", first, claimed, err)
	}
	if _, claimed, err := store.ClaimMessage(task.ID, "100:service", "event-1", "follow up"); err != nil || claimed {
		t.Fatal("in-flight message replayed", err)
	}
	if _, _, err := store.ClaimMessage(task.ID, "100:service", "event-1", "different"); err == nil {
		t.Fatal("changed body accepted")
	}
	pending, err := store.FinishMessage(first, "pending", "", "offline before delivery")
	if err != nil {
		t.Fatal(err)
	}
	retry, claimed, err := store.ClaimMessage(task.ID, "100:service", "event-1", "follow up")
	if err != nil || !claimed || retry.Status != "claimed" {
		t.Fatalf("safe retry: %+v %t %v", retry, claimed, err)
	}
	if _, err := store.FinishMessage(pending, "delivered", "thread", ""); err == nil {
		t.Fatal("stale claim completed")
	}
	uncertain, err := store.FinishMessage(retry, "uncertain", "thread", "delivery outcome unknown")
	if err != nil {
		t.Fatal(err)
	}
	if got, claimed, err := store.ClaimMessage(task.ID, "100:service", "event-1", "follow up"); err != nil || claimed || got != uncertain {
		t.Fatal("uncertain delivery replayed", err)
	}
}
