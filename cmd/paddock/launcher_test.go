package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestLauncherCLIAndIdentityMapping(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PADDOCK_HOME", home)
	if err := runLauncher([]string{"configure", "my-local", "--runtime", "host", "--agent", "claude", "--terminal", "screen"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := runIdentity([]string{"create", "me", "--launcher", "my-local"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runIdentity([]string{"show", "me", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var identity paddock.GitHubIdentity
	if err := json.Unmarshal(out.Bytes(), &identity); err != nil || identity.Launcher != "my-local" {
		t.Fatal(identity, err)
	}
	if err := runIdentity([]string{"configure", "me", "--launcher", "missing"}, io.Discard); err == nil {
		t.Fatal("accepted broken mapping")
	}
	if err := runIdentity([]string{"configure", "me", "--launcher", "default"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "launcher.json")
	if err := os.WriteFile(file, []byte(`{"runtime":"openshell","agent":"codex","read_only":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runLauncher([]string{"configure", "audit", "--file", file, "--codex-tui", "scrollback"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := runLauncher([]string{"configure", "audit", "--runtime", "host"}, io.Discard); err == nil {
		t.Fatal("host cannot enforce a read-only guard")
	}
	out.Reset()
	if err := runLauncher([]string{"show", "audit", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var c paddock.LauncherConfig
	if err := json.Unmarshal(out.Bytes(), &c); err != nil || c.Runtime != "openshell" || !c.ReadOnly || c.CodexTUI != "scrollback" {
		t.Fatal(c, err)
	}
	if err := runLauncher([]string{"configure", "audit", "--codex-tui", "invalid"}, io.Discard); err == nil {
		t.Fatal("invalid Codex TUI accepted")
	}
}

func TestNewFormLeavesLauncherDefaultsUnset(t *testing.T) {
	args := newTaskRunArgs(newTaskDraft{Repo: "/repo", Prompt: "work"}, map[string]string{"identity": "me", "launcher": "local", "codex-tui": "scrollback"})
	flags, pos, err := parse(args[1:], "json repo agent runtime identity launcher codex-tui")
	if err != nil || flags["identity"] != "me" || flags["launcher"] != "local" || flags["codex-tui"] != "scrollback" || flags["agent"] != "" || flags["runtime"] != "" || len(pos) != 1 || pos[0] != "work" {
		t.Fatal(flags, pos, err)
	}
}
