package paddock

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityLauncherResolution(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetLauncherConfig("my-local-agent", LauncherConfig{Runtime: "host", Agent: "claude", Terminal: "screen"}); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateIdentity("me", caller, "my-local-agent"); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateIdentity("unmapped", caller, ""); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	for _, tc := range []struct {
		name                     string
		opts                     RunOptions
		runtime, agent, terminal string
	}{
		{"default", RunOptions{}, "openshell", "codex", "auto"},
		{"unmapped identity", RunOptions{Identity: "unmapped"}, "openshell", "codex", "auto"},
		{"mapped identity", RunOptions{Identity: "me"}, "host", "claude", "screen"},
		{"explicit flags", RunOptions{Identity: "me", Runtime: "openshell", Agent: "codex", Backend: "rex"}, "openshell", "codex", "rex"},
		{"explicit config", RunOptions{Identity: "me", Launcher: "implementation"}, "openshell", "codex", "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if err := service.resolveLauncher(&opts); err != nil {
				t.Fatal(err)
			}
			if opts.Runtime != tc.runtime || opts.Agent != tc.agent || opts.Backend != tc.terminal {
				t.Fatalf("resolved %+v", opts)
			}
		})
	}
	if err = store.SetIdentityLauncher("me", "another caller", "default"); err == nil {
		t.Fatal("changed another caller's mapping")
	}
	if err = store.SetIdentityLauncher("me", caller, "missing"); err == nil {
		t.Fatal("saved a broken mapping")
	}
	if err = store.SetIdentityLauncher("me", caller, "default"); err != nil {
		t.Fatal(err)
	}
	if err = store.SetDefaultRuntime("host"); err != nil {
		t.Fatal(err)
	}
	opts := RunOptions{Identity: "me"}
	if err = service.resolveLauncher(&opts); err != nil || opts.Runtime != "host" {
		t.Fatal(opts, err)
	}
}

func TestLauncherSettingsDeterminePolicy(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	if err := store.SetLauncherConfig("audit", LauncherConfig{Runtime: "openshell", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetLauncherConfig("investigation", LauncherConfig{Runtime: "openshell"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"audit", "investigation"} {
		opts := RunOptions{Launcher: name}
		if err := (Service{Store: store}).resolveLauncher(&opts); err != nil {
			t.Fatal(err)
		}
		task := Task{Runtime: opts.Runtime, Launcher: &opts.launcher, Provenance: TaskProvenance{Profile: "investigation"},
			Session: Session{Agent: "codex"}, OpenShell: &OpenShellTask{Config: OpenShellConfig{Binary: "/bin/echo"}}}
		args := strings.Join(AgentCommand(task, "/state", "/paddock").Args, " ")
		if got := strings.Contains(args, investigationGuardDir+"/launch.py"); got != (name == "audit") {
			t.Fatal(name, args)
		}
	}
	for _, c := range []LauncherConfig{{Runtime: "unknown"}, {Agent: "unknown"}, {Terminal: "unknown"}, {Runtime: "host", ReadOnly: true}} {
		if err := store.SetLauncherConfig("invalid", c); err == nil {
			t.Fatal("accepted invalid launcher", c)
		}
	}
}

func TestHostIdentityLaunchPinsConfigAndRetries(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	c := LauncherConfig{Runtime: "host", Agent: "claude", Terminal: "screen"}
	if err = store.SetLauncherConfig("local", c); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateIdentity("me", caller, "local"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_CLAUDE_BIN", script(t, "exit 1"))
	t.Setenv("PADDOCK_SCREEN_BIN", script(t, `case "$1" in
 --version) echo 'Screen version fixture' ;;
 -ls) echo 'No Sockets found in /tmp/screen.' ;;
 *) exit 1 ;;
esac`))
	repo := repoFixture(t)
	gitTest(t, repo, "remote", "add", "origin", "https://github.com/team/repo.git")
	if err = store.RouteGitHubIdentity("team/repo", "me"); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Executable: filepath.Join(t.TempDir(), "paddock")}
	request := RunOptions{Repo: repo, Prompt: "local work", ExternalID: "dispatch-1"}
	task, err := service.Run(context.Background(), request)
	if err == nil || task.ID == "" {
		t.Fatal("fixture should fail after recording the host task", task, err)
	}
	if task.RuntimeName() != "host" || task.Session.Agent != "claude" || task.Provenance.Identity != "me" || task.Provenance.Launcher != "local" || task.Launcher == nil || *task.Launcher != c {
		t.Fatalf("incorrect host mapping: %+v", task)
	}
	if err = store.SetLauncherConfig("local", LauncherConfig{Runtime: "openshell"}); err != nil {
		t.Fatal(err)
	}
	again, err := service.Run(context.Background(), request)
	if err != nil || again.ID != task.ID || again.Launcher.Runtime != "host" {
		t.Fatal("retry changed the pinned launch", again, err)
	}
	if err = store.update(task.ID, func(v *Task) error { v.Launcher.Runtime = "openshell"; return nil }); err == nil {
		t.Fatal("mutated a task's pinned launcher")
	}
	if err := store.UpdateGitHub(func(v *GitHubState) error {
		identity := v.Identities["me"]
		identity.Disabled = true
		v.Identities["me"] = identity
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	request.ExternalID = "new-dispatch"
	if _, err := service.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal("ignored disabled repository identity", err)
	}

}
