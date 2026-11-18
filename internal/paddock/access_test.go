package paddock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func accessTaskFixture() Task {
	task := taskFixture()
	task.Runtime = "openshell"
	task.Status = "running"
	task.OpenShell = &OpenShellTask{ID: "fixture-sandbox-id", Name: "pd-fixture", Ready: true, Config: OpenShellConfig{Binary: "/bin/false", Gateway: "fixture", Workspace: "default"}}
	task.Session.Agent = "unsupported"
	return task
}

func TestAccessPersistenceAndDecisionUseOneStructuredLedger(t *testing.T) {
	home := t.TempDir()
	store := openTestStore(t, home)
	task := accessTaskFixture()
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	id := "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89"
	backend := &accessReviewBackend{inspection: OpenShellAccessInspection{SandboxID: task.OpenShell.ID, ExactRuleBound: true, Chunks: []OpenShellAccessChunk{
		{ID: id, Status: "pending", RuleName: "allow_pypi", Rule: json.RawMessage(`{"name":"allow_pypi","endpoints":[{"host":"pypi.org","ports":[443]}],"binaries":[{"path":"/opt/uv"}]}`), ReviewToken: "reviewed-token"},
		{ID: "ef5aaeb0-8a88-4430-be71-09eccbb10b19", Status: "rejected", RuleName: "other", Rule: json.RawMessage(`{"name":"other"}`), ReviewToken: "other-token"},
	}}}
	service := Service{Store: store, accessBackend: backend}
	requests, err := service.SyncAccess(context.Background(), task)
	if err != nil || len(requests) != 1 || requests[0].ID != id {
		t.Fatal(requests, err)
	}
	request, err := store.GetAccess("4cf929e5")
	if err != nil || request.Status != "pending" || request.Rule != "allow_pypi" || request.Endpoints != "pypi.org:443" || request.Binary != "/opt/uv" {
		t.Fatal(request, err)
	}
	if err := service.noPendingAccess(task.ID); err == nil {
		t.Fatal("pending ledger did not retain workspace")
	}
	request, err = service.DecideAccess(context.Background(), id, "approve", "")
	if err != nil || request.Status != "approved" || request.DecidedAt == "" || request.Decision != "approve" || request.AgentNotificationError == "" {
		t.Fatal(request, err)
	}
	if backend.approveCalls != 1 || backend.approvedToken != "reviewed-token" {
		t.Fatal("did not use captured structured token", backend.approveCalls, backend.approvedToken)
	}
	pending, err := store.ListAccess(task.ID, "pending")
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if err := service.noPendingAccess(task.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "access"))
	if !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
		t.Fatal("new access wrote a duplicate legacy inbox", entries, err)
	}
	if _, err := os.Stat(store.accessStatePath(task.ID)); err != nil {
		t.Fatal("missing authoritative ledger", err)
	}
}

func TestAccessDesktopNotificationClaimsShareLedger(t *testing.T) {
	t.Setenv("PADDOCK_DESKTOP_NOTIFICATIONS", "0")
	s, task, _, id := accessReviewFixture(t)
	// An integration can observe the request before the desktop poller. Discovery
	// must not consume its notification, but delivery intent must consume it once.
	if view := s.ShowAccessV1(context.Background(), task.ID, id, true); view.Error != nil {
		t.Fatal(view)
	}
	pending, err := s.SyncAccess(context.Background(), task)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	other := openTestStore(t, s.Store.Home)
	claimed := make(chan []AccessRequest, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{s.Store, other} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			r, e := store.claimAccessDesktop(task.ID, pending)
			claimed <- r
			errors <- e
		}(store)
	}
	wg.Wait()
	close(claimed)
	close(errors)
	count := 0
	for r := range claimed {
		count += len(r)
	}
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatal("desktop intent was not claimed exactly once", count)
	}
	again, err := s.SyncAccess(context.Background(), task)
	if err != nil || len(again) != 0 {
		t.Fatal(again, err)
	}
	state, err := s.Store.accessState(task.ID)
	if err != nil || state.Requests[id].NotifiedAt == "" {
		t.Fatal(state, err)
	}
	projected, err := s.Store.GetAccess(id)
	if err != nil || projected.NotifiedAt != state.Requests[id].NotifiedAt {
		t.Fatal(projected, err)
	}
}

func TestAccessRejectRequiresReason(t *testing.T) {
	s, task, _, id := accessReviewFixture(t)
	if _, err := s.SyncAccess(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideAccess(context.Background(), id, "reject", ""); err == nil {
		t.Fatal("accepted rejection without a reason")
	}
}
