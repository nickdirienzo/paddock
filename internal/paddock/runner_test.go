package paddock

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCodexTrustLaunchArguments(t *testing.T) {
	task := taskFixture()
	task.Session.AgentPath = "codex"
	task.Worktree = "/tmp/repo.with dots/a \"quoted\" \\ path\nand newline"
	original := AgentCommand(task, "/tmp/state", "/tmp/paddock")
	if len(original.Args) != 3 || original.Args[1] != "--" {
		t.Fatal("unexpected untrusted launch", original.Args)
	}
	task.Session.TrustProject = true
	trusted := AgentCommand(task, "/tmp/state", "/tmp/paddock")
	path, _ := json.Marshal(task.Worktree)
	expected := []string{"codex", "--config", "projects={" + string(path) + "={trust_level=\"trusted\"}}", "--", original.Args[2]}
	if !reflect.DeepEqual(trusted.Args, expected) {
		t.Fatalf("%q != %q", trusted.Args, expected)
	}
	if trusted.Dir != original.Dir || !reflect.DeepEqual(trusted.Env, original.Env) {
		t.Fatal("trust changed launch environment")
	}
	task.Session.Agent = "claude"
	task.Session.AgentPath = "claude"
	claude := AgentCommand(task, "/tmp/state", "/tmp/paddock")
	if len(claude.Args) != 3 || claude.Args[1] != "--" {
		t.Fatal("Codex trust flags leaked into Claude", claude.Args)
	}
}
