package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type nativeHook struct {
	Key         string `json:"key"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
	Enabled     bool   `json:"enabled"`
}

// Queries an isolated app-server; does not start a thread, run hooks, or call a model.
func nativeHooks(t *testing.T, cwd string, overrides ...string) []nativeHook {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := []string{"--config", "projects={" + tomlString(cwd) + "={trust_level=\"trusted\"}}"}
	for _, value := range overrides {
		if value != "" {
			args = append(args, "--config", value)
		}
	}
	args = append(args, "app-server", "--stdio")
	cmd := exec.CommandContext(ctx, "codex", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	encoder := json.NewEncoder(stdin)
	call := func(id int, method string, params any, result any) {
		t.Helper()
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for scanner.Scan() {
			var message struct {
				ID     int             `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				t.Fatal(err)
			}
			if message.ID != id {
				continue
			}
			if len(message.Error) > 0 {
				t.Fatalf("Codex %s: %s", method, message.Error)
			}
			if result != nil {
				if err := json.Unmarshal(message.Result, result); err != nil {
					t.Fatal(err)
				}
			}
			return
		}
		t.Fatalf("Codex %s ended without response: %v / %v", method, scanner.Err(), ctx.Err())
	}
	call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "paddock_hook_test", "version": "0"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil)
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data []struct {
			Hooks  []nativeHook      `json:"hooks"`
			Errors []json.RawMessage `json:"errors"`
		} `json:"data"`
	}
	call(2, "hooks/list", map[string]any{"cwds": []string{cwd}}, &response)
	if len(response.Data) != 1 || len(response.Data[0].Errors) > 0 {
		t.Fatalf("unexpected hook response: %+v", response)
	}
	return response.Data[0].Hooks
}

func TestLiveCodexHookTrust(t *testing.T) {
	if os.Getenv("PADDOCK_LIVE_CODEX") == "" {
		t.Skip("set PADDOCK_LIVE_CODEX=1 to verify against installed Codex without model calls")
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	repo := repoFixture(t)
	bare := filepath.Join(t.TempDir(), "repo.git")
	gitTest(t, repo, "clone", "--bare", repo, bare)
	repo = filepath.Join(t.TempDir(), "source")
	gitTest(t, bare, "worktree", "add", "-b", "hook-source", repo)
	r, err := discoverRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	repo = r.Root
	definition := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"true"}]}]}}`
	hookSource(t, repo, "hooks.json", definition)
	initial := nativeHooks(t, repo)
	if len(initial) != 1 || initial[0].TrustStatus != "untrusted" {
		t.Fatal("expected untrusted fixture", initial)
	}
	// Model the exact persisted state written by a native hook approval.
	save := func(state codexHookState) {
		b, err := toml.Marshal(map[string]any{"hooks": map[string]any{"state": map[string]codexHookState{initial[0].Key: state}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save(codexHookState{TrustedHash: initial[0].CurrentHash})
	worktree := filepath.Join(t.TempDir(), "linked.with spaces")
	gitTest(t, repo, "worktree", "add", "-b", "hook-live", worktree)
	worktree, _ = filepath.EvalSymlinks(worktree)
	hookSource(t, worktree, "hooks.json", definition)
	task := taskFixture()
	task.Worktree, task.CommonDir = worktree, r.CommonDir
	task.Session.TrustProject = true
	override, err := codexHookTrust(context.Background(), task)
	if err != nil || override == "" {
		t.Fatal(override, err)
	}
	trusted := nativeHooks(t, worktree, override)
	if len(trusted) != 1 || trusted[0].Key != filepath.Join(worktree, ".codex", "hooks.json")+":pre_tool_use:0:0" || trusted[0].TrustStatus != "trusted" || !trusted[0].Enabled {
		t.Fatal("Codex did not accept inherited hash", trusted)
	}
	// Even changes after Paddock's comparison must fail Codex's hash check.
	hookSource(t, worktree, "hooks.json", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"echo changed"}]}]}}`)
	changed := nativeHooks(t, worktree, override)
	if len(changed) != 1 || changed[0].TrustStatus != "modified" {
		t.Fatal("Codex trusted a changed definition", changed)
	}
	if next, err := codexHookTrust(context.Background(), task); err != nil || next != "" {
		t.Fatal("Paddock copied trust for changed file", next, err)
	}
	hookSource(t, worktree, "hooks.json", definition)
	disabled := false
	save(codexHookState{TrustedHash: initial[0].CurrentHash, Enabled: &disabled})
	override, err = codexHookTrust(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	hooks := nativeHooks(t, worktree, override)
	if len(hooks) != 1 || hooks[0].Enabled {
		t.Fatal("Codex lost disabled state", hooks)
	}
}
