package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestLifecycleContextFindsImmutableArchiveAfterNewSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PADDOCK_HOME", home)
	store, err := paddock.OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef01234567"
	oldID := "11111111-1111-4111-8111-111111111111"
	text := "HISTORICAL ARCHIVED CONVERSATION\nUSER: \"remember this task\"\nASSISTANT: \"kept context\"\n"
	hash := func(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
	a := paddock.ConversationArchive{Version: 2, TaskID: id, Agent: "claude", Runtime: "host", Worktree: "/old-worktree", Generation: 1, ConversationID: oldID, ContextText: text, ContextSHA256: hash([]byte(text)), SHA256: hash([]byte(text))}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	name := id + "-1-fixture.json"
	task := paddock.Task{ID: id, Runtime: "host", Status: "stopped", Session: paddock.Session{Agent: "claude", Generation: 2, ConversationID: "22222222-2222-4222-8222-222222222222"}, Archive: &paddock.TaskArchiveState{Phase: "revived", File: name, SHA256: hash(b), SourceGeneration: 1, SourceConversationID: oldID, PreviousWorktree: "/old-worktree", ContextSHA256: a.ContextSHA256}}
	if err = store.Insert(task); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "archives")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = runLifecycle([]string{"context", id[:8]}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != text {
		t.Fatal("lost task-ID context", out.String())
	}
}

func TestLifecycleExpiryCLIRejectsPolicyOptions(t *testing.T) {
	for _, args := range [][]string{{"expire", "task", "--worktree-ttl", "1h"}, {"expire", "task", "--disposable-directories", "build"}, {"configure", "--apply"}, {"context", "task", "--apply"}} {
		var out bytes.Buffer
		if err := runLifecycle(args, &out); err == nil {
			t.Fatal("accepted invalid lifecycle options", args)
		}
	}
}

func TestReviveHelpExposesFreshSessionCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"revive", "--help"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "paddock revive TASK_ID") {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), `--detach]\n`) {
		t.Fatal("literal newline escape in help")
	}
}
