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
		"Read the Paddock agent guide by running " + quote(exe) + " skill show before coordinating tasks or PR work. " +
		"Keep ordinary work in this task. When independent help is authorized, prefer native subagents for short reviews and checks. Use Paddock delegation only when a durable child task, separate worktree, or reconnectable session is needed, or the user requests it. " +
		"For such a child, run: PADDOCK_HOME=" + quote(home) + " " + quote(exe) + " delegate --parent " + t.ID + " --agent codex \"Describe the child task\". " +
		"Use --agent claude for Claude Code. Children start from the repository default branch; pass --base " + t.Branch + " to include committed parent work. Use paddock show to inspect children. " +
		"Communicate with existing Paddock agents through paddock message; never use terminal typing or simulated Enter as agent messaging."
	if t.RuntimeName() == "openshell" {
		prompt = t.Prompt + "\n\nPaddock task " + t.ID + ". This agent runs inside OpenShell. Work in " + t.Worktree + ". Use ordinary git and gh; GitHub requests authenticate as " + t.OpenShell.Identity.Login + " and are restricted to " + t.OpenShell.Repository + ". Keep work on branch " + t.Branch + ". Prefer native subagents for short reviews. The host owns lifecycle and PR watches; do not create nested Paddock tasks or try to access host credentials."
	}
	// Preserve the agent's configured authentication, approval, and sandbox policy.
	args := []string{}
	if t.RuntimeName() == "openshell" && t.Session.Agent == "codex" {
		// OpenShell is the isolation boundary; nested Bubblewrap cannot create
		// namespaces here. Keep native approval decisions enabled.
		args = append(args, "--remote", "unix:///home/sandbox/.codex/app-server-control/app-server-control.sock")
		// Remote resume restores the saved permissions and rejects overrides.
		if !t.Session.Resume {
			args = append(args, "--sandbox", "danger-full-access", "--ask-for-approval", "on-request")
		}
	}
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
	if t.Session.Resume {
		if t.Session.Agent == "codex" {
			args = append(args, "resume", t.Session.ConversationID)
		} else {
			args = append(args, "--resume", t.Session.ConversationID)
		}
		// Never replay the original assignment on resume.
		if t.Session.ResumeNotice != "" {
			args = append(args, "--", t.Session.ResumeNotice)
		}
	} else {
		if t.Session.Agent == "claude" && t.Session.ConversationID != "" {
			args = append(args, "--session-id", t.Session.ConversationID)
		}
		args = append(args, "--", prompt)
	}
	if t.RuntimeName() == "openshell" {
		remote := append([]string{"/usr/local/bin/paddock-agent", fmt.Sprint(t.Session.Generation), t.Session.AgentPath}, args...)
		c := sandboxExec(context.Background(), t, true, remote...)
		// The content-addressed transport copy has the agent's process name
		// so Rex can select native agent presentation through the wrapper.
		if t.OpenShell.AgentProxy != "" {
			c.Path = t.OpenShell.AgentProxy
		}
		c.Args[0] = t.Session.Agent
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c
	}
	c := exec.Command(t.Session.AgentPath, args...)
	c.Dir = t.Worktree
	c.Env = append(agentControlEnv(t), "PADDOCK_HOME="+home, "PADDOCK_TASK_ID="+t.ID, "PADDOCK_PARENT_TASK_ID="+t.ParentID,
		"PATH="+filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c
}

// RunAgent runs inside the terminal. Its lifetime is independent of `paddock run`.
func RunAgent(store *Store, id, exe string) int {
	return RunAgentGeneration(store, id, exe, 0)
}

func RunAgentGeneration(store *Store, id, exe string, generation int) int {
	t, err := store.Get(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	lease, err := store.taskLease(t.ID, "process")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer lease()
	if t.Session.Generation != generation {
		fmt.Fprintln(os.Stderr, "stale supervisor generation")
		return 1
	}
	if err = store.claimGeneration(t.ID, &generation); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	hookTrust := ""
	if t.RuntimeName() == "host" {
		miseCtx, miseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, miseErr := (Service{Store: store}).ReuseMiseTrust(miseCtx, t)
		miseCancel()
		if miseErr != nil {
			fmt.Fprintln(os.Stderr, "Could not reuse mise trust; mise may ask for approval:", miseErr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var hookErr error
		hookTrust, hookErr = codexHookTrust(ctx, t)
		cancel()
		if hookErr != nil {
			fmt.Fprintln(os.Stderr, "Could not reuse Codex hook approvals; review hooks in Codex:", hookErr)
		}
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
		bindingCtx, bindingCancel := context.WithCancel(context.Background())
		bindingDone := make(chan struct{})
		go func() { defer close(bindingDone); captureConversation(bindingCtx, store, t) }()
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
		bindingCancel()
		<-bindingDone
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
	if t.RuntimeName() == "openshell" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		// A disconnected transport does not prove that the remote agent exited.
		if current, getErr := store.Get(t.ID); getErr == nil {
			t = current
		}
		release, leaseErr := store.taskLease(t.ID, "operation")
		if leaseErr != nil {
			message += "; task operation still active; sandbox retained"
			code = 1
		} else {
			if e := stopCompletedSandbox(ctx, t); e != nil {
				message += "; " + e.Error()
				code = 1
			}
			release()
		}
		cancel()
	}
	if err = store.Finish(t.ID, code, message); err != nil {
		fmt.Fprintln(os.Stderr, "could not record task exit:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nPaddock task %s exited with status %d. Worktree: %s\n", t.ID, code, t.Worktree)
	return code
}
