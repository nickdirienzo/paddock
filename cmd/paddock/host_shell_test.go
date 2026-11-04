package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBareAttachOpensShellWithoutState(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "calls")
	rex := filepath.Join(root, "rex")
	text := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PADDOCK_TEST_CALLS"
case "$*" in
 *'whoami --json') printf '%s' '{"endpoint":"unix:///rex"}' ;;
 *'new host-shell '*) printf '%s' '{"session_id":"session:shell","initial_windows":[{"window_id":"window:shell","block_ids":["block:shell"]}]}' ;;
 *'actions --json') printf '%s' '{"actions":[{"name":"session.select"}]}' ;;
 *'do session.select '*) exit 0 ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(rex, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_TEST_CALLS", log)
	t.Setenv("PADDOCK_REX_BIN", rex)
	t.Setenv("PADDOCK_HOME", filepath.Join(root, "must-not-be-created"))
	t.Setenv("REX_SESSION", "")
	t.Setenv("REX_BLOCK", "")
	var out bytes.Buffer
	if err := run([]string{"attach", "--client", "client:chosen"}, &out); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "new host-shell") || !strings.Contains(string(calls), "-C client:chosen do session.select session_id=session:shell") {
		t.Fatal(string(calls))
	}
	if _, err = os.Stat(os.Getenv("PADDOCK_HOME")); !os.IsNotExist(err) {
		t.Fatalf("bare attach created state: %v", err)
	}
}

func TestShellSetupValidatesArguments(t *testing.T) {
	t.Setenv("PADDOCK_TASK_ID", "")
	t.Setenv("PADDOCK_HOME", filepath.Join(t.TempDir(), "not-created"))
	for _, args := range [][]string{{"shell-setup"}, {"shell-setup", "a", "b"}, {"shell-setup", "--task", "a"}} {
		var out bytes.Buffer
		if err := run(args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, command := range []string{"shell-setup"} {
		var out bytes.Buffer
		if err := run([]string{command, "--help"}, &out); err != nil {
			t.Fatal(err)
		}
	}
}
