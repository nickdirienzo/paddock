package paddock

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeHistoricalAccess(t *testing.T, s *Store, r AccessRequest) string {
	t.Helper()
	dir := filepath.Join(s.Home, "access")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, r.ID+".json")
	b, err := json.Marshal(accessRecord{Version: 1, AccessRequest: r})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAccessLegacyHistoryIsReadOnlyAndNeverGrantAuthority(t *testing.T) {
	s, task, backend, id := accessReviewFixture(t)
	historical := AccessRequest{ID: id, TaskID: task.ID, Sandbox: task.OpenShell.Name, Status: "pending", Endpoints: "old.example:443", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z", NotifiedAt: "2026-01-02T00:00:01Z", DesktopNotificationError: "old desktop error"}
	path := writeHistoricalAccess(t, s.Store, historical)
	before, _ := os.ReadFile(path)
	requests, err := s.SyncAccess(context.Background(), task)
	if err != nil || len(requests) != 0 {
		t.Fatal("historical desktop notification was duplicated", requests, err)
	}
	request, err := s.Store.GetAccess(id)
	if err != nil || request.CreatedAt != historical.CreatedAt || request.NotifiedAt != historical.NotifiedAt || request.DesktopNotificationError != historical.DesktopNotificationError || request.Endpoints == historical.Endpoints {
		t.Fatal(request, err)
	}
	if _, err = s.DecideAccess(context.Background(), id, "approve", ""); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only compatibility rewrote original history", err)
	}
	if backend.approveCalls != 1 {
		t.Fatal(backend.approveCalls)
	}
	// A historical-only request cannot supply a token or an immutable snapshot.
	other := historical
	other.ID = "ef5aaeb0-8a88-4430-be71-09eccbb10b19"
	writeHistoricalAccess(t, s.Store, other)
	if _, err = s.DecideAccess(context.Background(), other.ID, "approve", ""); err == nil {
		t.Fatal("granted from historical text")
	}
	if backend.approveCalls != 1 {
		t.Fatal("historical display data reached backend")
	}
	pending, err := s.Store.ListAccess(task.ID, "pending")
	if err != nil || len(pending) != 1 || pending[0].ID != other.ID {
		t.Fatal("historical pending record lost retention", pending, err)
	}
}

func TestAccessLegacyCompatibilityRejectsUnsafeRecords(t *testing.T) {
	for _, scenario := range []string{"symlink", "oversized", "identity", "version", "foreign task"} {
		t.Run(scenario, func(t *testing.T) {
			s, task, _, id := accessReviewFixture(t)
			record := AccessRequest{ID: id, TaskID: task.ID, Sandbox: task.OpenShell.Name, Status: "pending"}
			path := writeHistoricalAccess(t, s.Store, record)
			switch scenario {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, bytes.Repeat([]byte(" "), maxLegacyAccessBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "identity":
				record.ID = "ef5aaeb0-8a88-4430-be71-09eccbb10b19"
				b, _ := json.Marshal(accessRecord{Version: 1, AccessRequest: record})
				_ = os.WriteFile(path, b, 0600)
			case "version":
				b, _ := json.Marshal(accessRecord{Version: 2, AccessRequest: record})
				_ = os.WriteFile(path, b, 0600)
			case "foreign task":
				record.TaskID = "../outside"
				b, _ := json.Marshal(accessRecord{Version: 1, AccessRequest: record})
				_ = os.WriteFile(path, b, 0600)
			}
			if _, err := s.Store.ListAccess(task.ID, ""); err == nil {
				t.Fatal("accepted unsafe compatibility record")
			}
		})
	}
}

func TestAccessLegacyAndLedgerPrefixAmbiguityIsRejected(t *testing.T) {
	s, task, _, id := accessReviewFixture(t)
	if _, err := s.SyncAccess(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	other := AccessRequest{ID: "4cf929e5-ffff-43c4-a6e4-b0b7efb9ec89", TaskID: task.ID, Sandbox: task.OpenShell.Name, Status: "pending"}
	writeHistoricalAccess(t, s.Store, other)
	if _, err := s.Store.GetAccess(id[:8]); err == nil {
		t.Fatal("ambiguous projection prefix selected a request")
	}
	if _, err := s.Store.GetAccess(id); err != nil {
		t.Fatal(err)
	}
}
