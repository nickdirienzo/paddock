package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveIntegrityAndInterruptedPublication(t *testing.T) {
	s, task, _ := gcFixture(t)
	a, err := s.ArchiveConversation(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.NativeRestoreQualified || a.ContextText == "" || len(a.Transcript) != 0 {
		t.Fatal(a)
	}
	dir := filepath.Join(s.Store.Home, "archives")
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal(info.Mode())
	}
	path := filepath.Join(dir, task.ID+".json")
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	// A killed atomic writer leaves only an ignored temp file; committed archive survives.
	if err := os.WriteFile(filepath.Join(dir, ".paddock-interrupted.tmp"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(task.ID); err != nil {
		t.Fatal(err)
	}
	a.Transcript = []byte("corrupt")
	if err := writeJSONAtomic(path, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(task.ID); err == nil {
		t.Fatal("accepted corrupt archive")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(task.ID); err == nil {
		t.Fatal("accepted missing archive")
	}
	if _, err := s.ArchiveConversation(context.Background(), task.ID); err != nil {
		t.Fatal("retry failed", err)
	}
}

func TestArchiveBeforeRemovalAndFailureRecovery(t *testing.T) {
	s, task, checks := gcFixture(t)
	// Block archive publication after all ordinary GC checks have passed.
	if err := os.WriteFile(filepath.Join(s.Store.Home, "archives"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := gcOne(t, s, true, checks); r.Removed {
		t.Fatal(r)
	}
	if _, err := os.Stat(task.Worktree); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Store.Home, "archives")); err != nil {
		t.Fatal(err)
	}
	if r := gcOne(t, s, true, checks); !r.Removed {
		t.Fatal(r)
	}
	if _, err := s.VerifyArchive(task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestUnshippedTaskAndChangedShipmentRetained(t *testing.T) {
	s, task, checks := gcFixture(t)
	if err := s.Store.update(task.ID, func(v *Task) error { v.ShippedAt = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if r := gcOne(t, s, true, checks); r.Eligible || r.Removed {
		t.Fatal(r)
	}
	if err := s.MarkShipped(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	gitTest(t, task.Worktree, "commit", "--allow-empty", "-m", "later work")
	if r := gcOne(t, s, true, checks); r.Eligible || r.Removed {
		t.Fatal(r)
	}
}

func TestArchiveRejectsSymlinkTranscript(t *testing.T) {
	s, task, _ := gcFixture(t)
	path := filepath.Join(task.Session.AgentHome, "projects", "test", task.Session.ConversationID+".jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveConversation(context.Background(), task.ID); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestSandboxArchiveReaderAndCodexLayout(t *testing.T) {
	s, task, _ := gcFixture(t)
	task.Session.Agent = "codex"
	if err := s.Store.update(task.ID, func(v *Task) error { v.Session.Agent = "codex"; return nil }); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(task.Session.AgentHome, "sessions", "2026", "11", "16")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-11-16-"+task.Session.ConversationID+".jsonl")
	data := []byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"fixture conversation\"}]}}\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := s.ArchiveConversation(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.ContextText, "fixture conversation") || len(a.Transcript) != 0 {
		t.Fatal("missing readable chat or retained unnecessary native state")
	}
	// Exercise the exact sandbox extraction program on disposable files, without a sandbox.
	b, err := exec.Command("python3", "-c", archiveReadScript, path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := json.Unmarshal(b, &got); err != nil || string(got) != string(data) {
		t.Fatal(err)
	}
}
