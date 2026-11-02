package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentDoesNotReceiveGitHubAppCredentials(t *testing.T) {
	_, pem := appKeyFixture(t)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", string(pem))
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY_FILE", "/private/fixture/key.pem")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pem))
	t.Setenv("ARBITRARY_PEM", string(pem))
	t.Setenv("ARBITRARY_TOKEN", "ghs_fixture_installation_token")
	t.Setenv("GH_TOKEN", "ghs_fixture_installation_token")
	t.Setenv("KEEP_ME", "ordinary-agent-setting")
	task := taskFixture()
	task.Session.AgentPath = script(t, "env\n")
	task.Worktree = t.TempDir()
	command := AgentCommand(task, "/tmp/state", "/tmp/paddock")
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PRIVATE KEY", "ghs_fixture", "/private/fixture/key.pem", "PADDOCK_GITHUB_PRIVATE_KEY", "ARBITRARY_PEM", "ARBITRARY_TOKEN"} {
		if strings.Contains(string(output), secret) || strings.Contains(strings.Join(command.Args, " "), secret) {
			t.Fatal("credential reached agent process or prompt")
		}
	}
	if !strings.Contains(string(output), "KEEP_ME=ordinary-agent-setting") {
		t.Fatal("unrelated environment was lost")
	}
	for _, env := range [][]string{agentControlEnv(task), gitEnv()} {
		if strings.Contains(strings.Join(env, "\n"), "PRIVATE KEY") || strings.Contains(strings.Join(env, "\n"), "ghs_fixture") {
			t.Fatal("credential reached agent-control or Git subprocess")
		}
	}
	// Exercise the terminal CLI boundary as a real process, not just a filtered slice.
	envProgram := script(t, "env\n")
	terminalOutput, err := outputCommandForTest(context.Background(), envProgram)
	if err != nil || strings.Contains(terminalOutput, "PRIVATE KEY") || strings.Contains(terminalOutput, "ghs_fixture") {
		t.Fatal("credential reached terminal subprocess", err)
	}
	serialized, _ := json.Marshal(task)
	if strings.Contains(string(serialized), "PRIVATE KEY") {
		t.Fatal("key persisted in task")
	}
}
func outputCommandForTest(ctx context.Context, path string) (string, error) {
	b, err := output(ctx, path)
	return string(b), err
}

func TestQueuedAppReplyNeedsNoCredential(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	if err := s.UpdateGitHub(func(v *GitHubState) error {
		v.Identity = GitHubIdentity{Mode: "app", Login: "team-robot[bot]", App: &GitHubAppConfig{AppID: 42, InstallationID: 99, KeyEnvironment: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := testComment(1, "Please fix this", 7)
	event := GitHubEvent{ID: githubEventID(w, c), WatchKey: w.Key(), TaskID: w.TaskID, Comment: c, Plan: Plan{Action: "message"}, Status: "done"}
	if err := g.saveEvent(event); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_TASK_ID", w.TaskID)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", "")
	post, err := g.EnqueueReply(event.ID, "Fix complete.")
	if err != nil || post.Status != "queued" {
		t.Fatal(post, err)
	}
	again, err := g.EnqueueReply(event.ID, "Fix complete.")
	if err != nil || again.ID != post.ID {
		t.Fatal("duplicate queued reply", err)
	}
	t.Setenv("PADDOCK_TASK_ID", "")
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.replies) != 1 {
		t.Fatal("queued reply not sent exactly once")
	}
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.replies) != 1 {
		t.Fatal("queued reply repeated")
	}
}

func TestAgentCLIQueuesAppReplyWithoutKey(t *testing.T) {
	_, _, s, w, _ := githubFixture(t)
	c := testComment(1, "Please fix this", 7)
	event := GitHubEvent{ID: githubEventID(w, c), WatchKey: w.Key(), TaskID: w.TaskID, Comment: c, Plan: Plan{Action: "message"}, Status: "done"}
	if err := s.UpdateGitHub(func(v *GitHubState) error {
		v.Identity = GitHubIdentity{Mode: "app", Login: "team-robot[bot]", App: &GitHubAppConfig{AppID: 42, InstallationID: 99, KeyEnvironment: true}}
		v.Events[event.ID] = event
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	exe := buildCLI(t)
	command := exec.Command(exe, "github", "reply", event.ID, "Done through agent CLI.")
	command.Env = append(credentialFreeEnvironment(os.Environ()), "PADDOCK_HOME="+s.Home, "PADDOCK_TASK_ID="+w.TaskID)
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "queued") {
		t.Fatalf("%v: %s", err, output)
	}
	b, err := os.ReadFile(filepath.Join(s.Home, "github.json"))
	if err != nil || strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatal("unexpected journal", err)
	}
}
