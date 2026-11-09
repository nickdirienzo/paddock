//go:build darwin || linux

package paddock

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Opt-in qualification against the installed Claude, with its ordinary project
// trust, admission rules, login and approvals. No GitHub writes or terminal input.
func TestLiveHostClaudeInbox(t *testing.T) {
	worktree := os.Getenv("PADDOCK_LIVE_CLAUDE_CWD")
	if worktree == "" {
		t.Skip("set PADDOCK_LIVE_CLAUDE_CWD to a trusted directory for installed-Claude qualification")
	}
	agent, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		home = filepath.Join(userHome, ".claude")
	}
	task := taskFixture()
	task.Worktree = worktree
	task.Session = Session{Agent: "claude", AgentPath: agent, AgentHome: home, ConversationID: newConversationID(), Generation: 1}
	command := exec.Command(agent, "--session-id", task.Session.ConversationID, "--", "This is a disposable Paddock inbox test. Do not use tools or change files. Reply only READY_FOR_PADDOCK.")
	command.Dir, command.Env = worktree, agentControlEnv(task)
	inbox, cleanup, err := prepareHostClaudeInbox(task, command)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "claude-output")
	if err != nil {
		command.Process.Kill()
		command.Wait()
		terminal.Close()
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() { defer close(readDone); io.Copy(output, terminal) }()
	defer func() {
		command.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { command.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			command.Process.Kill()
			<-done
		}
		terminal.Close()
		<-readDone
		output.Close()
		if t.Failed() {
			data, _ := os.ReadFile(output.Name())
			t.Logf("Claude terminal: %s", data)
		}
	}()
	inbox.PID = command.Process.Pid
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	var history string
	for ctx.Err() == nil {
		paths, _ := filepath.Glob(filepath.Join(home, "projects", "*", task.Session.ConversationID+".jsonl"))
		if len(paths) == 1 {
			data, _ := os.ReadFile(paths[0])
			if strings.Contains(string(data), "READY_FOR_PADDOCK") {
				history = paths[0]
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if history == "" {
		t.Fatal("native Claude did not finish startup with its existing trust settings")
	}
	if _, err := deliverHostClaude(ctx, task, inbox, "Do not use tools or change files. Reply only PADDOCK_HOST_INBOX_OK."); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		data, _ := os.ReadFile(history)
		if strings.Contains(string(data), `"text":"PADDOCK_HOST_INBOX_OK"`) {
			t.Log("native inbox admitted the message and Claude replied in the same conversation", task.Session.ConversationID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Claude admitted the message but did not produce the expected native reply")
}
