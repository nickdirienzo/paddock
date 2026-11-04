package paddock

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hostShellFixture(t *testing.T, source string) (Task, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "rex-calls")
	task := taskFixture()
	task.Session = Session{Backend: "rex", ID: "session:task", BlockID: "block:task", Endpoint: "unix:///rex", WindowID: "window:agent"}
	task.Session.TerminalPath = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'whoami --json') printf '%s' '{"endpoint":"unix:///rex"}' ;;
 *'client ls --json') printf '%s' '{"clients":[{"client_id":"client:mini","connection_state":"connected","info":{"kind":"app"}},{"client_id":"client:laptop","connection_state":"connected","info":{"kind":"app"}}]}' ;;
 *'block inspect '*) printf '%s' '{"block":{"block_id":"`+source+`","owner":"client:laptop"}}' ;;
 *'session inspect session:task --json') printf '%s' '{"session_id":"session:task"}' ;;
 *'session inspect session:caller --json') printf '%s' '{"session_id":"session:caller"}' ;;
 *'session inspect '*) exit 1 ;;
 *'window new '*) printf '%s' '{"window_id":"window:shell","block_ids":["block:shell"]}' ;;
 *'new host-shell '*) printf '%s' '{"session_id":"session:new","initial_windows":[{"window_id":"window:shell","block_ids":["block:shell"]}]}' ;;
 *'actions --json') printf '%s' '{"actions":[{"name":"session.select"}]}' ;;
 *'do session.select '*) exit 0 ;;
 *) exit 1 ;;
esac
`)
	return task, log
}

func TestOpenHostShellUsesInvokingPane(t *testing.T) {
	for _, source := range []string{"session:caller", "session:stale"} {
		t.Run(source, func(t *testing.T) {
			task, log := hostShellFixture(t, "block:caller")
			t.Setenv("PADDOCK_REX_BIN", task.Session.TerminalPath)
			t.Setenv("REX_SESSION", source)
			t.Setenv("REX_BLOCK", "block:caller")
			t.Setenv("REX_CLIENT", "")
			opened, err := (Service{}).OpenHostShell(context.Background(), nil, "")
			if err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(log)
			want := "window new -s session:caller Host Shell"
			if source == "session:stale" {
				want = "new host-shell"
			}
			if !strings.Contains(string(calls), want) || !strings.Contains(string(calls), "-C client:laptop do session.select") || opened.WindowID != "window:shell" {
				t.Fatalf("opened=%+v calls=%s", opened, calls)
			}
		})
	}
}

func TestShellRequestIsTaskScopedAndIdempotent(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	task, log := hostShellFixture(t, "block:task")
	task.Runtime = "openshell"
	acks := filepath.Join(t.TempDir(), "acknowledgements")
	task.OpenShell = &OpenShellTask{Name: "sandbox:owned", Ready: true, Config: OpenShellConfig{Binary: script(t, `printf '%s\n' "$*" >> `+quote(acks)+"\n")}}
	liveDir, _ := sandboxShareFixture(t, &task)
	t.Setenv("MISE_IGNORED_CONFIG_PATHS", "/host/already-ignored")
	service := Service{Store: store, Executable: "/host/paddock"}
	request := "1234567890abcdef12345678"
	for range 2 {
		if err := service.handleShellRequest(context.Background(), task, request); err != nil {
			t.Fatal(err)
		}
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "window new ") != 1 {
		t.Fatalf("duplicate shell: %s", calls)
	}
	for _, want := range []string{"block inspect block:task --json -s session:task", "Host Shell -c " + liveDir, "PADDOCK_TASK_ID=" + task.ID, "PATH=/host:", "-C client:laptop do session.select"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("missing %q: %s", want, calls)
		}
	}
	for _, bad := range []string{"", "../../../host", "$(touch INJECTION)", strings.Repeat("a", 25)} {
		if err := service.handleShellRequest(context.Background(), task, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	wantIgnore := "MISE_IGNORED_CONFIG_PATHS=/host/already-ignored" + string(os.PathListSeparator) + filepath.Join(os.Getenv("HOME"), "OrbStack", "docker", "containers")
	if !strings.Contains(string(calls), wantIgnore) || strings.Contains(string(calls), "mise trust") {
		t.Fatalf("inspection shell must skip sandbox configs without changing trust: %s", calls)
	}
	// A host crash after recording intent must not open another tab.
	result := shellRequestResult{ID: request, CreatedAt: time.Now(), Status: "opening"}
	if err := writeJSONAtomic(filepath.Join(store.Home, "shell-requests", task.ID+".json"), result); err != nil {
		t.Fatal(err)
	}
	if err := service.handleShellRequest(context.Background(), task, request); err != nil {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(log)
	if strings.Count(string(calls), "window new ") != 1 {
		t.Fatalf("retried uncertain shell: %s", calls)
	}
}

func TestSandboxShellClientAndInstaller(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for sandbox client fixture")
	}
	root, home := t.TempDir(), t.TempDir()
	installer := strings.ReplaceAll(strings.ReplaceAll(installSandboxShell, "/sandbox/.paddock", root), "/home/sandbox", home)
	client := strings.ReplaceAll(sandboxShellClient, "/sandbox/.paddock", root)
	snapshot := filepath.Join(home, ".codex/shell_snapshots/thread.sh")
	if err = os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(snapshot, []byte("export EXISTING=yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if b, e := exec.Command(python, "-c", installer, client).CombinedOutput(); e != nil {
			t.Fatalf("%v: %s", e, b)
		}
	}
	b, _ := os.ReadFile(snapshot)
	if !strings.HasPrefix(string(b), "export EXISTING=yes\n") || strings.Count(string(b), "# Paddock host-shell shortcut") != 1 {
		t.Fatal(string(b))
	}
	cmd := exec.Command(python, filepath.Join(root, "bin/paddock"), "attach")
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var request string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, e := exec.Command(python, "-c", strings.ReplaceAll(readShellRequest, "/sandbox/.paddock", root)).Output()
		if e != nil {
			t.Fatal(e)
		}
		request = strings.TrimSpace(string(b))
		if validID(request) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !validID(request) {
		t.Fatalf("no request: %q", request)
	}
	ack := strings.ReplaceAll(acknowledgeShellRequest, "/sandbox/.paddock", root)
	if b, e := exec.Command(python, "-c", ack, request, "opened").CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, b)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if b, e := exec.Command(python, filepath.Join(root, "bin/paddock"), "run", "arbitrary host command").CombinedOutput(); e == nil {
		t.Fatalf("accepted unsupported operation: %s", b)
	}
}
