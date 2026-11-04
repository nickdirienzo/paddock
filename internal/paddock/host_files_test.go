package paddock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sandboxShareFixture(t *testing.T, task *Task) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if task.OpenShell == nil {
		task.OpenShell = &OpenShellTask{}
	}
	task.OpenShell.ID = "d890ff34-31da-4b7b-9bff-9b66ad689608"
	task.OpenShell.Name = "pd-owned"
	task.OpenShell.Config.Workspace = "default"
	task.Worktree = "/sandbox/worktree"
	dir := filepath.Join(home, "OrbStack/docker/containers/workload-owned/sandbox/worktree")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "docker-calls")
	t.Setenv("PADDOCK_DOCKER_BIN", script(t, `printf '%s\n' "$*" >> `+quote(log)+"\nprintf '%s\\n' workload-owned\n"))
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, log
}

func TestHostSandboxWorktreeIsLiveAndTaskScoped(t *testing.T) {
	task := taskFixture()
	dir, log := sandboxShareFixture(t, &task)
	view, err := hostSandboxWorktree(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || view != canonical {
		t.Fatalf("view=%q want %q err=%v", view, canonical, err)
	}
	file := filepath.Join(dir, "artifact.md")
	for _, version := range []string{"first", "updated by agent"} {
		if err = os.WriteFile(file, []byte(version), 0600); err != nil {
			t.Fatal(err)
		}
		b, e := os.ReadFile(filepath.Join(view, "artifact.md"))
		if e != nil || string(b) != version {
			t.Fatalf("view is stale: %s %v", b, e)
		}
	}
	calls, _ := os.ReadFile(log)
	for _, filter := range []string{"sandbox-id=" + task.OpenShell.ID, "sandbox-name=pd-owned", "sandbox-workspace=default", "isolation-role=sandbox"} {
		if !strings.Contains(string(calls), "--filter label=openshell.ai/"+filter) {
			t.Fatalf("missing %q: %s", filter, calls)
		}
	}
}

func TestHostSandboxWorktreeRejectsUnavailableOrAmbiguousViews(t *testing.T) {
	for _, tc := range []struct {
		name, listing   string
		missing, escape bool
	}{
		{name: "share unavailable", missing: true},
		{name: "no running workload"},
		{name: "multiple workloads", listing: "one\ntwo\n"},
		{name: "invalid container name", listing: "../another-container\n"},
		{name: "worktree symlink escape", listing: "workload-owned\n", escape: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, log := hostShellFixture(t, "block:task")
			dir, _ := sandboxShareFixture(t, &task)
			if tc.missing {
				t.Setenv("HOME", t.TempDir())
			}
			t.Setenv("PADDOCK_DOCKER_BIN", script(t, "printf '%s' "+quote(tc.listing)+"\n"))
			if tc.escape {
				if err := os.Rename(dir, dir+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			}
			store := openTestStore(t, t.TempDir())
			_, err := (Service{Store: store, Executable: "/host/paddock"}).OpenHostShell(context.Background(), &task, "")
			if err == nil {
				t.Fatal("opened a shell without the correct live view")
			}
			calls, _ := os.ReadFile(log)
			if strings.Contains(string(calls), "window new") {
				t.Fatalf("opened shell despite error: %s", calls)
			}
		})
	}
}
