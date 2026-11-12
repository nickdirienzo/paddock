package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestClaudeProjectTrustOnlyChangesIsolatedProjectState(t *testing.T) {
	config := filepath.Join(t.TempDir(), ".claude.json")
	original := map[string]any{
		"hasCompletedOnboarding": true,
		"mcpServers":             map[string]any{"fixture": map[string]any{"command": "true"}},
		"projects":               map[string]any{"/another/project": map[string]any{"hasTrustDialogAccepted": false}},
	}
	data, _ := json.Marshal(original)
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-c", claudeProjectTrustScript, config, "/sandbox/worktree")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed trust: %v: %s", err, output)
	}
	updated, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err = json.Unmarshal(updated, &actual); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"hasCompletedOnboarding": true,
		"mcpServers":             map[string]any{"fixture": map[string]any{"command": "true"}},
		"projects": map[string]any{
			"/another/project":  map[string]any{"hasTrustDialogAccepted": false},
			"/sandbox/worktree": map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("unexpected Claude config: %#v", actual)
	}
	if info, err := os.Stat(config); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("Claude config permissions: %v %v", info, err)
	}
}

func TestClaudeProjectTrustRequiresExplicitGrantAndIsolatedWorktree(t *testing.T) {
	for _, task := range []Task{
		{Session: Session{Agent: "claude"}},
		{Session: Session{Agent: "codex", TrustProject: true}},
	} {
		if err := installClaudeProjectTrust(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	if err := installClaudeProjectTrust(context.Background(), Task{Session: Session{Agent: "claude", TrustProject: true}, Worktree: "/elsewhere"}); err == nil {
		t.Fatal("trusted Claude outside the isolated worktree")
	}
}
