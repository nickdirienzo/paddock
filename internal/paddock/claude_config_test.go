package paddock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostClaudeCapturesConfigSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PADDOCK_CLAUDE_BIN", script(t, "exit 1\n"))
	// Fail after task creation so no real terminal or agent is launched.
	t.Setenv("PADDOCK_SCREEN_BIN", script(t, `case "$1" in
 --version) echo 'Screen version fixture' ;;
 -ls) echo 'No Sockets found in fixture.' ;;
 *) exit 1 ;;
esac`))
	repo, err := ResolveRepository(context.Background(), repoFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{"", filepath.Join(home, ".claude"), t.TempDir(), "relative-config"} {
		t.Run("config="+config, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", config)
			store := openTestStore(t, t.TempDir())
			_, err := (Service{Store: store, Executable: "/tmp/paddock"}).runHost(context.Background(), RunOptions{Agent: "claude", Backend: "screen"}, repo)
			if err == nil {
				t.Fatal("fixture terminal should fail to launch")
			}
			tasks, err := store.List("")
			if err != nil || len(tasks) != 1 {
				t.Fatal(tasks, err)
			}
			session := tasks[0].Session
			wantDir, wantHome := "", filepath.Join(home, ".claude")
			if config != "" {
				wantDir, err = filepath.Abs(config)
				if err != nil {
					t.Fatal(err)
				}
				wantHome = wantDir
			}
			if session.AgentHome != wantHome || session.ClaudeConfigDir == nil || *session.ClaudeConfigDir != wantDir {
				t.Fatalf("persisted home/config = %q/%v; want %q/%q", session.AgentHome, session.ClaudeConfigDir, wantHome, wantDir)
			}
		})
	}
}

func TestClaudeConfigEnvironmentOnLaunchAndResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultHome := filepath.Join(home, ".claude")
	customHome := filepath.Join(home, "custom")
	defaultConfig, explicitDefault, customConfig := "", defaultHome, customHome
	for _, tc := range []struct {
		name, agentHome, want string
		config                *string
	}{
		{"default", defaultHome, "", &defaultConfig},
		{"explicit default directory", defaultHome, defaultHome, &explicitDefault},
		{"custom directory", customHome, customHome, &customConfig},
		{"legacy default", defaultHome, "", nil},
		{"legacy custom", customHome, customHome, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A terminal server or a later caller can have a different config.
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "unrelated"))
			task := taskFixture()
			task.Session.Agent = "claude"
			task.Session.AgentHome = tc.agentHome
			task.Session.ClaudeConfigDir = tc.config
			// Preserve the unset/explicit distinction through saved task JSON.
			data, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &task); err != nil {
				t.Fatal(err)
			}
			for _, resume := range []bool{false, true} {
				task.Session.Resume = resume
				commands := [][]string{
					AgentCommand(task, "/tmp/state", "/tmp/paddock").Env,
					agentCommandContext(context.Background(), task, "--version").Env,
				}
				for _, env := range commands {
					var entries []string
					for _, entry := range env {
						if strings.HasPrefix(entry, "CLAUDE_CONFIG_DIR=") {
							entries = append(entries, entry)
						}
					}
					if tc.want == "" && len(entries) != 0 || tc.want != "" && (len(entries) != 1 || entries[0] != "CLAUDE_CONFIG_DIR="+tc.want) {
						t.Fatalf("resume=%t: config entries %q; want %q", resume, entries, tc.want)
					}
				}
			}
		})
	}
}

// Exercise the installed CLI's config lookup without host credentials or a model.
func TestNativeClaudeMCPConfigSelection(t *testing.T) {
	agent := os.Getenv("PADDOCK_NATIVE_CLAUDE")
	if agent == "" {
		t.Skip("set PADDOCK_NATIVE_CLAUDE to check installed Claude MCP config lookup")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	defaultHome, customHome := filepath.Join(home, ".claude"), filepath.Join(home, "custom")
	for path, marker := range map[string]string{
		filepath.Join(home, ".claude.json"):        "default-config",
		filepath.Join(defaultHome, ".claude.json"): "explicit-default-config",
		filepath.Join(customHome, ".claude.json"):  "custom-config",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONAtomic(path, map[string]any{"mcpServers": map[string]any{
			"paddock-config-fixture": map[string]any{"type": "stdio", "command": "/usr/bin/true", "args": []string{marker}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ configDir, agentHome, marker string }{
		{"", defaultHome, "default-config"},
		{defaultHome, defaultHome, "explicit-default-config"},
		{customHome, customHome, "custom-config"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "unrelated"))
			task := taskFixture()
			task.Worktree = t.TempDir()
			task.Session = Session{Agent: "claude", AgentPath: agent, AgentHome: tc.agentHome, ClaudeConfigDir: &tc.configDir}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			data, err := agentCommandContext(ctx, task, "mcp", "get", "paddock-config-fixture").CombinedOutput()
			if err != nil || !strings.Contains(string(data), "Args: "+tc.marker) {
				t.Fatalf("Claude loaded the wrong user MCP config: %v\n%s", err, data)
			}
		})
	}
}
