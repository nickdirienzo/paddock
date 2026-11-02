package paddock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func hookSource(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, ".codex", name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInheritedHookDecisions(t *testing.T) {
	for _, name := range []string{"hooks.json", "config.toml"} {
		t.Run(name, func(t *testing.T) {
			source, target, other := t.TempDir(), t.TempDir(), t.TempDir()
			sourceKey := hookSource(t, source, name, "same hook definition") + ":pre_tool_use:0:0"
			targetKey := hookSource(t, target, name, "same hook definition") + ":pre_tool_use:0:0"
			otherKey := hookSource(t, other, name, "same hook definition") + ":pre_tool_use:0:0"
			trusted := codexHookState{TrustedHash: "sha256:" + strings.Repeat("a", 64)}
			saved := map[string]codexHookState{sourceKey: trusted}
			inherit := func() map[string]codexHookState { return inheritedHookStates(target, []string{source, other}, saved) }
			if got := inherit(); len(got) != 1 || got[targetKey].TrustedHash != trusted.TrustedHash {
				t.Fatal("did not carry saved approval", got)
			}
			// A local cleared or disabled decision must win over sibling approvals.
			saved[targetKey] = codexHookState{}
			if got := inherit(); len(got) != 0 {
				t.Fatal("overrode local untrust", got)
			}
			delete(saved, targetKey)
			disabled := false
			saved[otherKey] = codexHookState{Enabled: &disabled}
			if got := inherit(); len(got) != 0 {
				t.Fatal("conflicting enable decisions must require review", got)
			}
			delete(saved, sourceKey)
			if got := inherit(); len(got) != 1 || got[targetKey].Enabled == nil || *got[targetKey].Enabled {
				t.Fatal("lost disabled decision", got)
			}
			saved[sourceKey] = trusted
			saved[otherKey] = codexHookState{TrustedHash: "sha256:" + strings.Repeat("b", 64)}
			if got := inherit(); len(got) != 0 {
				t.Fatal("conflicting hashes must require review", got)
			}
			delete(saved, otherKey)
			hookSource(t, target, name, "changed hook definition")
			if got := inherit(); len(got) != 0 {
				t.Fatal("trusted changed hook source", got)
			}
			hookSource(t, target, name, "same hook definition")
			saved[sourceKey] = codexHookState{TrustedHash: "not-a-hash"}
			if got := inherit(); len(got) != 0 {
				t.Fatal("copied malformed trust hash", got)
			}
		})
	}
}

func TestCodexHookTrustRepositoryScope(t *testing.T) {
	repo := repoFixture(t)
	worktree := filepath.Join(t.TempDir(), "worktree.with 'quotes and spaces")
	gitTest(t, repo, "worktree", "add", "-b", "hook-test", worktree)
	worktree, _ = filepath.EvalSymlinks(worktree)
	r, err := discoverRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey := hookSource(t, r.Root, "hooks.json", "unchanged") + ":pre_tool_use:0:0"
	targetKey := hookSource(t, worktree, "hooks.json", "unchanged") + ":pre_tool_use:0:0"
	other := repoFixture(t)
	otherKey := hookSource(t, other, "hooks.json", "unchanged") + ":pre_tool_use:0:0"
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	task := taskFixture()
	task.CommonDir, task.Worktree = r.CommonDir, worktree
	task.Session.TrustProject = true
	hash := "sha256:" + strings.Repeat("a", 64)
	save := func(states map[string]codexHookState) {
		t.Helper()
		b, err := toml.Marshal(map[string]any{"hooks": map[string]any{"state": states}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save(map[string]codexHookState{otherKey: {TrustedHash: hash}})
	if override, err := codexHookTrust(context.Background(), task); err != nil || override != "" {
		t.Fatal("trust crossed repositories", override, err)
	}
	save(map[string]codexHookState{sourceKey: {TrustedHash: hash}})
	override, err := codexHookTrust(context.Background(), task)
	if err != nil || override == "" {
		t.Fatal("missing override", override, err)
	}
	var parsed struct {
		Hooks struct{ State map[string]codexHookState }
	}
	if err := toml.Unmarshal([]byte(override), &parsed); err != nil || parsed.Hooks.State[targetKey].TrustedHash != hash {
		t.Fatal("incorrect TOML path encoding", override, parsed, err)
	}
	command := AgentCommand(task, "/tmp/state", "/tmp/paddock", override)
	if len(command.Args) != 7 || command.Args[3] != "--config" || command.Args[4] != override {
		t.Fatal("hook state not passed to Codex", command.Args)
	}
	task.Session.Agent = "claude"
	if got, err := codexHookTrust(context.Background(), task); err != nil || got != "" {
		t.Fatal("Codex state leaked to Claude", got, err)
	}
	if args := AgentCommand(task, "/tmp/state", "/tmp/paddock", override).Args; len(args) != 3 {
		t.Fatal("Codex flags leaked to Claude", args)
	}
	task.Session.Agent = "codex"
	task.Session.TrustProject = false
	if got, err := codexHookTrust(context.Background(), task); err != nil || got != "" {
		t.Fatal("hook trust applied without repository opt-in", got, err)
	}
}
