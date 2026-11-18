package paddock

import (
	"strings"
	"testing"
)

func guidanceFixture(t *testing.T) (*Store, Task) {
	t.Helper()
	store := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Runtime = "openshell"
	task.OpenShell = &OpenShellTask{}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	return store, task
}

func TestAuthoredMessageGuidanceFrozenAtAdmission(t *testing.T) {
	store, task := guidanceFixture(t)
	authored := "Continue the same report. Literal $(keep) and `text`.\n"
	receipt, claimed, err := store.ClaimAuthoredMessage(task.ID, "501:integration", "request-1", authored)
	if err != nil || !claimed {
		t.Fatalf("claim: %+v %v", receipt, err)
	}
	if receipt.Body != authored || !strings.HasPrefix(receipt.NativeBody(), authored) || strings.Count(receipt.NativeBody(), "/sandbox/.paddock/access-guide.md") != 1 || receipt.DeliveryBody == "" {
		t.Fatalf("authored input or native guidance changed: %+v", receipt)
	}
	if _, err = store.FinishMessage(receipt, "pending", "", "Definitely unsent fixture"); err != nil {
		t.Fatal(err)
	}
	retry, claimed, err := store.ClaimAuthoredMessage(task.ID, "501:integration", "request-1", authored)
	if err != nil || !claimed || retry.NativeBody() != receipt.NativeBody() || retry.Body != authored {
		t.Fatalf("retry drifted: %+v %v", retry, err)
	}
	if _, _, err = store.ClaimAuthoredMessage(task.ID, "501:integration", "request-1", authored+"changed"); err == nil {
		t.Fatal("changed authored input reused an idempotency key")
	}
	uncertain, err := store.FinishMessage(retry, "uncertain", "thread-1", "fixture outcome unknown")
	if err != nil {
		t.Fatal(err)
	}
	replay, claimed, err := store.ClaimAuthoredMessage(task.ID, "501:integration", "request-1", authored)
	if err != nil || claimed || replay != uncertain {
		t.Fatalf("uncertain body replayed: %+v %v", replay, err)
	}
}

func TestAuthoredMessagePreservesOlderFrozenGuidanceAndLegacyReceipts(t *testing.T) {
	for _, oldDelivery := range []string{"", "Follow up\n\nPaddock runtime guidance from an older binary."} {
		t.Run(oldDelivery, func(t *testing.T) {
			store, task := guidanceFixture(t)
			original := MessageReceipt{Version: 1, TaskID: task.ID, Caller: "501:integration", ExternalID: "existing", Body: "Follow up", DeliveryBody: oldDelivery, Status: "pending"}
			if err := writeJSONAtomic(store.messageReceiptPath(task.ID, original.Caller, original.ExternalID), original); err != nil {
				t.Fatal(err)
			}
			retry, claimed, err := store.ClaimAuthoredMessage(task.ID, original.Caller, original.ExternalID, original.Body)
			if err != nil || !claimed || retry.DeliveryBody != oldDelivery || retry.NativeBody() != original.NativeBody() {
				t.Fatalf("stored native body was rebuilt during upgrade: %+v %v", retry, err)
			}
			if retry.NativeBody() == AuthoredMessageBody(task, original.Body) {
				t.Fatal("old delivery should not acquire the current guide pointer")
			}
		})
	}
}

func TestRuntimeGuidanceIsOwnedByLaunchAndAuthoredInputOnly(t *testing.T) {
	store, task := guidanceFixture(t)
	initial := agentPrompt(task, "unused", "paddock")
	followup := AuthoredMessageBody(task, "New authored followup")
	const pointer = "/sandbox/.paddock/access-guide.md"
	if strings.Count(initial, pointer) != 1 || strings.Count(followup, pointer) != 1 {
		t.Fatal("Paddock launch/followup do not own one guide pointer")
	}
	internal, claimed, err := store.ClaimMessage(task.ID, "sandbox:child", "report", "Internal result notification")
	if err != nil || !claimed || internal.DeliveryBody != "" || internal.NativeBody() != internal.Body || strings.Contains(internal.NativeBody(), pointer) {
		t.Fatalf("internal notification was augmented: %+v %v", internal, err)
	}
	task.Runtime = "host"
	if got := AuthoredMessageBody(task, "Host authored input"); got != "Host authored input" {
		t.Fatal("host input received sandbox guidance")
	}
	host := taskFixture()
	if err = store.Insert(host); err != nil {
		t.Fatal(err)
	}
	receipt, claimed, err := store.ClaimAuthoredMessage(host.ID, "501:integration", "host-followup", "Host authored input")
	if err != nil || !claimed || receipt.DeliveryBody != "" || receipt.NativeBody() != receipt.Body {
		t.Fatalf("host receipt changed delivery: %+v %v", receipt, err)
	}
	task.Runtime = "openshell"
	for _, empty := range []string{"", " \n\t"} {
		if got := AuthoredMessageBody(task, empty); got != empty {
			t.Fatal("guide turned empty authored input into a message")
		}
	}
}
