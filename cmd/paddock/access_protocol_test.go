package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func accessCLIState(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PADDOCK_HOME", home)
	store, err := paddock.OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := "0123456789abcdef01234567"
	if err = store.Insert(paddock.Task{ID: id, Runtime: "openshell", Status: "stopped", Prompt: "fixture"}); err != nil {
		t.Fatal(err)
	}
	return home, id
}
func TestAccessCLIWireEmptyListingAndBoundErrors(t *testing.T) {
	_, id := accessCLIState(t)
	var out bytes.Buffer
	if err := run([]string{"access", "list", "--task", id, "--status", "all", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var list paddock.AccessListResponse
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || list.ProtocolVersion != 1 || list.Requests == nil || len(list.Features) == 0 || list.TaskID != id {
		t.Fatal(out.String(), err)
	}
	for _, args := range [][]string{{"access", "list", "--task", id[:8], "--json"}, {"access", "show", "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89", "--task", id, "--json"}, {"access", "reconcile", "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89", "--task", id, "--json"}} {
		out.Reset()
		if err := run(args, &out); err == nil {
			t.Fatal("accepted unavailable/unbound request", args)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatal("missing structured failure", out.String())
		}
		if args[1] == "reconcile" {
			var response paddock.AccessDecisionResponse
			if err := json.Unmarshal(out.Bytes(), &response); err != nil || response.Error == nil || response.Error.Code == "no_decision" {
				t.Fatal("unknown request cannot prove intent absence", out.String(), err)
			}
		}
	}
}
func TestAccessCLIDecisionFileRequiresExactTaskAndVersion(t *testing.T) {
	_, id := accessCLIState(t)
	path := filepath.Join(t.TempDir(), "decision.json")
	in := paddock.AccessDecisionInput{ProtocolVersion: 1, TaskID: id, Revision: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Decision: "approve", ExternalID: "action-1", Actor: paddock.AccessActor{Source: "arnold-slack", WorkspaceID: "T1", UserID: "U1"}, Reason: "fixture"}
	b, _ := json.Marshal(in)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"access", "decide", "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89", "--task", "fedcba9876543210fedcba98", "--decision-file", path, "--json"}, &out); err == nil {
		t.Fatal("task mismatch accepted")
	}
	var failure paddock.AccessDecisionResponse
	if err := json.Unmarshal(out.Bytes(), &failure); err != nil || failure.ProtocolVersion != 1 || failure.Error == nil || failure.Receipt != nil {
		t.Fatal(out.String(), err)
	}
	in.ProtocolVersion = 2
	b, _ = json.Marshal(in)
	_ = os.WriteFile(path, b, 0600)
	out.Reset()
	if err := run([]string{"access", "decide", "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89", "--decision-file", path, "--json"}, &out); err == nil {
		t.Fatal("future protocol accepted")
	}
	if err := json.Unmarshal(out.Bytes(), &failure); err != nil || failure.Error.Code != "protocol_unsupported" {
		t.Fatal(out.String(), err)
	}
}
