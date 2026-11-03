package paddock

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMiseConfigSnapshot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		for _, path := range []string{"mise.toml", "mise.production.toml", ".mise/config.toml", ".config/mise/config.toml", ".tool-versions", ".node-version", "package.json"} {
			full := filepath.Join(root, path)
			if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(path), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := miseConfigSnapshot(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := miseConfigSnapshot(b)
	if err != nil || len(first) != 7 || !reflect.DeepEqual(first, second) {
		t.Fatal("identical configs differ", err)
	}
	if err = os.WriteFile(filepath.Join(b, "mise.production.toml"), []byte("changed inactive environment"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err = miseConfigSnapshot(b)
	if err != nil || reflect.DeepEqual(first, second) {
		t.Fatal("missed changed environment config", err)
	}
	if err = os.Remove(filepath.Join(b, "mise.toml")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(a, "mise.toml"), filepath.Join(b, "mise.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err = miseConfigSnapshot(b); err == nil {
		t.Fatal("accepted symlink config")
	}
}

func TestLiveMiseTrustReuse(t *testing.T) {
	if os.Getenv("PADDOCK_LIVE_MISE") == "" {
		t.Skip("set PADDOCK_LIVE_MISE=1 for isolated tests against installed mise")
	}
	mise, err := executable("PADDOCK_MISE_BIN", "mise")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_MISE_BIN", mise)
	for _, key := range []string{"MISE_CONFIG_DIR", "MISE_STATE_DIR", "MISE_DATA_DIR", "MISE_CACHE_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	for _, key := range []string{"MISE_CONFIG_FILE", "MISE_OVERRIDE_CONFIG_FILENAMES", "MISE_DEFAULT_CONFIG_FILENAME", "MISE_TRUSTED_CONFIG_PATHS", "MISE_IGNORED_CONFIG_PATHS"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MISE_PARANOID", "false")
	global := filepath.Join(t.TempDir(), "config.toml")
	if err = os.WriteFile(global, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISE_GLOBAL_CONFIG_FILE", global)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seed := repoFixture(t)
	if err = os.WriteFile(filepath.Join(seed, "mise.toml"), []byte("[env]\nPADDOCK_MISE_FIXTURE = 'ok'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, seed, "add", "mise.toml")
	gitTest(t, seed, "commit", "-m", "mise fixture")
	bare := filepath.Join(t.TempDir(), "repo.git")
	gitTest(t, seed, "clone", "--bare", seed, bare)
	source := filepath.Join(t.TempDir(), "source")
	gitTest(t, bare, "worktree", "add", "-b", "source", source)
	if _, err = miseOutput(ctx, mise, source, "trust", filepath.Join(source, "mise.toml")); err != nil {
		t.Fatal(err)
	}
	repo, err := discoverRepository(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, t.TempDir())
	service := Service{Store: store}
	newTask := func(name string) Task {
		root := filepath.Join(t.TempDir(), name)
		gitTest(t, bare, "worktree", "add", "-b", name, root)
		root, _ = filepath.EvalSymlinks(root)
		return Task{Worktree: root, CommonDir: repo.CommonDir}
	}
	matching := newTask("matching")
	if copied, err := service.ReuseMiseTrust(ctx, matching); err != nil || copied {
		t.Fatal("unapproved Paddock repo inherited trust", err)
	}
	if _, err = store.SetRepoTrust(ctx, source, true); err != nil {
		t.Fatal(err)
	}
	if copied, err := service.ReuseMiseTrust(ctx, matching); err != nil || !copied {
		t.Fatal("identical sibling did not inherit", err)
	}
	if b, err := miseOutput(ctx, mise, matching.Worktree, "env", "--json"); err != nil || !strings.Contains(string(b), "PADDOCK_MISE_FIXTURE") {
		t.Fatal("config remained blocked", err)
	}
	if copied, err := service.ReuseMiseTrust(ctx, matching); err != nil || copied {
		t.Fatal("repeated native trust mutation", err)
	}
	// Exercise the production supervisor path: the probe's first command needs
	// mise config trust and cannot answer a prompt (no input is provided).
	launch := newTask("launch")
	probe := taskFixture()
	probe.Worktree, probe.CommonDir = launch.Worktree, launch.CommonDir
	probe.Session.AgentPath = script(t, quote(mise)+" env --json >/dev/null\n")
	if err = store.Insert(probe); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, buildCLI(t), "__run", probe.ID)
	cmd.Env = append(os.Environ(), "PADDOCK_HOME="+store.Home)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("supervisor probe: %v %s", err, output)
	}
	completed, err := store.Get(probe.ID)
	if err != nil || completed.Status != "stopped" {
		t.Fatal("probe did not complete", err)
	}
	changed := newTask("changed")
	if err = os.WriteFile(filepath.Join(changed.Worktree, "mise.toml"), []byte("[env]\nPADDOCK_MISE_FIXTURE = 'different'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if copied, err := service.ReuseMiseTrust(ctx, changed); err != nil || copied {
		t.Fatal("changed config inherited", err)
	}
	if _, err = miseOutput(ctx, mise, changed.Worktree, "env", "--json"); err == nil {
		t.Fatal("changed config was usable without approval")
	}
	ignored := newTask("ignored")
	if _, err = miseOutput(ctx, mise, ignored.Worktree, "trust", "--ignore", filepath.Join(ignored.Worktree, "mise.toml")); err != nil {
		t.Fatal(err)
	}
	if copied, err := service.ReuseMiseTrust(ctx, ignored); err != nil || copied {
		t.Fatal("explicit ignore was overridden", err)
	}
	paranoid := newTask("paranoid")
	t.Setenv("MISE_PARANOID", "true")
	if copied, err := service.ReuseMiseTrust(ctx, paranoid); err != nil || copied {
		t.Fatal("paranoid trust was changed", err)
	}
	t.Log("Native mise accepted identical sibling config; changed, ignored, unapproved and paranoid cases preserved")
}

// Explicit maintenance opt-in for existing tasks. It uses exactly the same
// approval, repository membership, native trust and content checks as startup.
func TestLiveMiseExistingTasks(t *testing.T) {
	ids := os.Getenv("PADDOCK_LIVE_MISE_TASKS")
	if ids == "" {
		t.Skip("set PADDOCK_LIVE_MISE_TASKS to reuse existing approvals for specific task IDs")
	}
	home, err := StateHome()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := Service{Store: store}
	for _, id := range strings.Split(ids, ",") {
		task, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		copied, err := service.ReuseMiseTrust(ctx, task)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Task %s: copied existing approval=%t", task.ID, copied)
	}
}
