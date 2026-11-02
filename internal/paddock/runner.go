package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func AgentCommand(t Task, home, exe string, overrides ...string) *exec.Cmd {
	prompt := t.Prompt + "\n\nPaddock context: task " + t.ID + ". Work in this task's current Git worktree. " +
		"To delegate a child task, run: PADDOCK_HOME=" + quote(home) + " " + quote(exe) + " delegate --parent " + t.ID + " --agent claude \"Describe the child task\". " +
		"Use --agent codex to delegate to Codex. Children start from the repository default branch; pass --base " + t.Branch + " to include committed parent work. Use paddock show to inspect children."
	// Preserve the agent's configured authentication, approval, and sandbox policy.
	args := []string{}
	if t.Session.Agent == "codex" && t.Session.TrustProject {
		// An inline TOML table preserves dots and quotes in the path. Codex's
		// CLI dotted-key parser does not parse quoted path components.
		path, _ := json.Marshal(t.Worktree)
		args = append(args, "--config", "projects={"+string(path)+"={trust_level=\"trusted\"}}")
		for _, override := range overrides {
			if override != "" {
				args = append(args, "--config", override)
			}
		}
	}
	c := exec.Command(t.Session.AgentPath, append(args, "--", prompt)...)
	c.Dir = t.Worktree
	c.Env = append(credentialFreeEnvironment(os.Environ()), "PADDOCK_HOME="+home, "PADDOCK_TASK_ID="+t.ID, "PADDOCK_PARENT_TASK_ID="+t.ParentID,
		"PATH="+filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c
}

// RunAgent runs inside the terminal. Its lifetime is independent of `paddock run`.
func RunAgent(store *Store, id, exe string) int {
	t, err := store.Get(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err = store.Claim(t.ID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	hookTrust, hookErr := codexHookTrust(ctx, t)
	cancel()
	if hookErr != nil {
		fmt.Fprintln(os.Stderr, "Could not reuse Codex hook approvals; review hooks in Codex:", hookErr)
	}
	c := AgentCommand(t, store.Home, exe, hookTrust)
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	code := 1
	message := ""
	restore, grouped, err := prepareAgentTerminal(c)
	if err == nil {
		err = c.Start()
	}
	if err != nil {
		message = err.Error()
	} else {
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		var timer <-chan time.Time
	wait:
		for {
			select {
			case err = <-done:
				break wait
			case sig := <-signals:
				// Interactive agents own a separate foreground group. Forward any
				// signal aimed at the supervisor to that group, including descendants.
				if grouped || sig != os.Interrupt {
					_ = signalAgent(c, grouped, sig)
				}
				if sig != os.Interrupt {
					timer = time.After(5 * time.Second)
				}
			case <-timer:
				_ = signalAgent(c, grouped, os.Kill)
				timer = nil
			}
		}
		code = c.ProcessState.ExitCode()
		if code < 0 {
			code = 128
		}
		if err != nil {
			message = err.Error()
		}
	}
	if restoreErr := restore(); restoreErr != nil {
		fmt.Fprintln(os.Stderr, "could not restore terminal foreground:", restoreErr)
	}
	if err = store.Finish(t.ID, code, message); err != nil {
		fmt.Fprintln(os.Stderr, "could not record task exit:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nPaddock task %s exited with status %d. Worktree: %s\n", t.ID, code, t.Worktree)
	return code
}
