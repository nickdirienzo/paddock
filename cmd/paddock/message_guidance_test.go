package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestMessageCLIFreezesPaddockRuntimeGuidanceWithoutStartingAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PADDOCK_HOME", home)
	store, err := paddock.OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	// Unsupported native messaging stops before any remote execution/resume.
	task := paddock.Task{ID: "0123456789abcdef01234567", Runtime: "openshell", Session: paddock.Session{Agent: "unsupported"}, OpenShell: &paddock.OpenShellTask{}}
	if err = store.Insert(task); err != nil {
		t.Fatal(err)
	}
	store.Close()
	var output bytes.Buffer
	args := []string{"message", task.ID, "New Slack-authored input", "--external-id", "authored-1", "--json"}
	if err = run(args, &output); err == nil {
		t.Fatal("unsupported agent unexpectedly accepted delivery")
	}
	var receipt paddock.MessageReceipt
	if err = json.Unmarshal(output.Bytes(), &receipt); err != nil {
		t.Fatalf("missing receipt: %s %v", output.String(), err)
	}
	if receipt.Body != "New Slack-authored input" || !strings.Contains(receipt.DeliveryBody, "/sandbox/.paddock/access-guide.md") || receipt.Status != "uncertain" {
		t.Fatalf("CLI did not freeze its own runtime guidance: %+v", receipt)
	}
	output.Reset()
	if err = run(args, &output); err != nil {
		t.Fatal(err)
	}
	var replay paddock.MessageReceipt
	if err = json.Unmarshal(output.Bytes(), &replay); err != nil || replay != receipt {
		t.Fatalf("CLI retry changed delivery body or receipt: %+v %v", replay, err)
	}
}
