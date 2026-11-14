package paddock

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCodexTUICommandModes(t *testing.T) {
	for _, runtime := range []string{"host", "openshell"} {
		for _, agent := range []string{"codex", "claude"} {
			for _, resume := range []bool{false, true} {
				for _, mode := range []string{"", "default", "scrollback", "fullscreen"} {
					t.Run(runtime+"/"+agent+"/"+mode+"/resume="+map[bool]string{true: "true", false: "false"}[resume], func(t *testing.T) {
						task := taskFixture()
						task.Runtime = runtime
						task.Launcher = &LauncherConfig{CodexTUI: mode}
						task.Session.Agent, task.Session.AgentPath = agent, agent
						task.Session.Resume, task.Session.ConversationID = resume, "saved-conversation"
						if runtime == "openshell" {
							task.OpenShell = &OpenShellTask{Config: OpenShellConfig{Binary: "/bin/echo"}}
						}
						args := AgentCommand(task, "/state", "/paddock").Args
						joined := strings.Join(args, " ")
						if slices.Contains(args, "--no-alt-screen") != (agent == "codex" && mode == "scrollback") {
							t.Fatal(args)
						}
						if strings.Contains(joined, "tui.fullscreen_transcript=true") != (agent == "codex" && mode == "fullscreen") {
							t.Fatal(args)
						}
						if strings.Contains(joined, "tui.fullscreen_transcript=false") != (agent == "codex" && mode == "scrollback") {
							t.Fatal(args)
						}
						if resume && !slices.Contains(args, "saved-conversation") {
							t.Fatal("lost conversation", args)
						}
						if resume && slices.Contains(args, task.Prompt) {
							t.Fatal("replayed assignment", args)
						}
					})
				}
			}
		}
	}
}

func TestCodexTUILauncherAndSessionOverrides(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	if err := store.SetLauncherConfig("remote", LauncherConfig{Runtime: "host", CodexTUI: "scrollback"}); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	for _, mode := range []string{"", "fullscreen", "default"} {
		opts := RunOptions{Launcher: "remote", CodexTUI: mode}
		if err := service.resolveLauncher(&opts); err != nil {
			t.Fatal(err)
		}
		want := mode
		if want == "" {
			want = "scrollback"
		}
		if opts.CodexTUI != want || opts.launcher.CodexTUI != want {
			t.Fatal(opts)
		}
	}
	if err := store.SetLauncherConfig("bad", LauncherConfig{CodexTUI: "bad"}); err == nil {
		t.Fatal("invalid mode accepted")
	}
	task := taskFixture()
	task.Session.Agent = "codex"
	task.Launcher = &LauncherConfig{CodexTUI: "scrollback"}
	task.Session.CodexTUI = "default"
	if args := codexTUIArgs(task); len(args) != 0 {
		t.Fatal("default did not clear launcher override", args)
	}
	task.Session.CodexTUI = "fullscreen"
	if args := codexTUIArgs(task); !reflect.DeepEqual(args, []string{"--config", `tui.alternate_screen="always"`, "--config", "tui.fullscreen_transcript=true"}) {
		t.Fatal(args)
	}
}

func TestCodexTUIResumeRefusesToChangeRunningAgent(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Status, task.Session.Agent, task.Session.Backend = "running", "codex", "screen"
	task.Session.TerminalPath = script(t, `printf '123.%s (Detached)\n' `+quote(task.Session.Name))
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	_, err := (Service{Store: store}).ResumeWithCodexTUI(context.Background(), task.ID, "scrollback")
	if err == nil || !strings.Contains(err.Error(), "terminal is running") {
		t.Fatal(err)
	}
	saved, err := store.Get(task.ID)
	if err != nil || saved.Session.CodexTUI != "" || saved.Session.Generation != task.Session.Generation {
		t.Fatal(saved, err)
	}
}

func TestCodexTUIExternalRetryUsesOriginalLauncher(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: newID(), Prompt: "work", Runtime: "host", Launcher: &LauncherConfig{CodexTUI: "scrollback"},
		Session: Session{Agent: "codex", CodexTUI: "fullscreen"}, Provenance: TaskProvenance{Caller: caller, ExternalID: "dispatch"}}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	if got, err := service.Run(context.Background(), RunOptions{Prompt: "work", ExternalID: "dispatch", CodexTUI: "scrollback"}); err != nil || got.ID != task.ID {
		t.Fatal(got, err)
	}
	if _, err := service.Run(context.Background(), RunOptions{Prompt: "work", ExternalID: "dispatch", CodexTUI: "fullscreen"}); err == nil {
		t.Fatal("changed dispatch options accepted")
	}
}

func TestCodexTUIResumeKeepsConversation(t *testing.T) {
	service, task, _ := gcFixture(t)
	service.Executable = buildCLI(t)
	control := t.TempDir()
	active, finish, argsFile := filepath.Join(control, "active"), filepath.Join(control, "finish"), filepath.Join(control, "args")
	agent := script(t, `if [ "$1" = app-server ]; then
 while IFS= read -r line; do
  case "$line" in
   *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
   *'"id":2'*) printf '%s\n' '{"id":2,"result":{"thread":{"id":"`+task.Session.ConversationID+`","cwd":"`+canonicalWorkspacePath(task.Worktree)+`"}}}' ;;
  esac
 done
else
 printf '%s\n' "$@" > `+quote(argsFile)+`
 while [ ! -f `+quote(finish)+` ]; do sleep 0.05; done
 rm -f `+quote(active)+`
fi`)
	terminal := script(t, `case "$1" in
 -ls) if [ -f `+quote(active)+` ]; then printf '123.%s (Detached)\n' "$(cat `+quote(active)+`)"; else echo 'No Sockets found in /tmp/screen.'; fi ;;
 -c) printf '%s' "$4" > `+quote(active)+`; shift 4; "$@" > /dev/null 2>&1 & ;;
 *) exit 1 ;;
esac`)
	if err := service.Store.UpdateSession(task.ID, func(v *Session) { v.Agent, v.AgentPath, v.TerminalPath = "codex", agent, terminal }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(finish, nil, 0600)
		waitFor(t, func() bool { current, _ := service.Store.Get(task.ID); return current.Status != "running" })
	})
	resumed, err := service.ResumeWithCodexTUI(context.Background(), task.ID, "scrollback")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Session.ConversationID != task.Session.ConversationID || resumed.Session.CodexTUI != "scrollback" || !resumed.Session.Resume {
		t.Fatal(resumed)
	}
	waitFor(t, func() bool { _, err := os.Stat(argsFile); return err == nil })
	data, err := os.ReadFile(argsFile)
	if err != nil || !strings.HasPrefix(string(data), "--no-alt-screen\n--config\ntui.fullscreen_transcript=false\nresume\n"+task.Session.ConversationID) {
		t.Fatal(string(data), err)
	}
}
