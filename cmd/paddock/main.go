package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const help = `Paddock — a local control plane for coding agents

  paddock run "prompt" [--agent codex|claude] [--terminal auto|rex|screen]
                      [--runtime openshell|host] [--repo path] [--base ref] [--json] [--detach] [--client Rex-client-id]
  paddock delegate "prompt" [--parent id] [run options]
  paddock message <id> "message"
  paddock tasks [--json]
  paddock show <id> [--json]
  paddock attach <id> [--client Rex-client-id]
  paddock resume <id> [--json] [--detach] [--client Rex-client-id]
  paddock runtime show|configure|check
  paddock gc [--apply] [--json]
  paddock lifecycle show|configure
  paddock github --help
  paddock orchestrate --help
  paddock planner configure|show|reset
  paddock skill setup|status|remove|show
  paddock repo add <name> <path>
  paddock repo list [--json]
  paddock repo remove <name>
  paddock repo trust <name-or-path>
  paddock repo untrust <name-or-path>

Delegate inherits the parent from PADDOCK_TASK_ID when --parent is omitted.
Task IDs accept unique prefixes. State: PADDOCK_HOME or ~/.local/state/paddock.
Rex attach selects a session in a connected Rex app. Screen attaches here.
Run opens its Rex session automatically; --detach or --json keeps it in the background.
--repo accepts a registered name, a working-tree path, or a bare-repository path.
`

// Accept options before or after positional arguments, and -- for literal text.
func parse(args []string, allowed string) (map[string]string, []string, error) {
	opts := map[string]string{}
	pos := []string{}
	literal := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if literal {
			pos = append(pos, a)
			continue
		}
		if a == "--" {
			literal = true
			continue
		}
		if a == "-h" {
			a = "--help"
		}
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		key, val, has := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if key == "help" {
			opts[key] = "true"
			continue
		}
		if !strings.Contains(" "+allowed+" ", " "+key+" ") {
			return nil, nil, fmt.Errorf("unknown option --%s", key)
		}
		if _, ok := opts[key]; ok {
			return nil, nil, fmt.Errorf("duplicate option --%s", key)
		}
		if key == "json" || key == "queue" || key == "detach" || key == "dry-run" || key == "apply" {
			if has {
				return nil, nil, fmt.Errorf("--%s takes no value", key)
			}
			opts[key] = "true"
			continue
		}
		if !has {
			i++
			if i == len(args) {
				return nil, nil, fmt.Errorf("--%s requires a value", key)
			}
			val = args[i]
		}
		if val == "" {
			return nil, nil, fmt.Errorf("--%s requires a non-empty value", key)
		}
		opts[key] = val
	}
	return opts, pos, nil
}

func emit(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
func printTask(w io.Writer, t paddock.Task) {
	fmt.Fprintf(w, "Task: %s\nStatus: %s\nAgent: %s\nRepository: %s\nBase: %s (%s)\nBranch: %s\nWorktree: %s\nTerminal: %s\nSession: %s\n", t.ID, t.Status, t.Session.Agent, t.Repo, t.Base, t.BaseCommit, t.Branch, t.Worktree, t.Session.Backend, t.Session.ID)
	fmt.Fprintln(w, "Runtime:", t.RuntimeName())
	if t.OpenShell != nil {
		fmt.Fprintf(w, "Sandbox: %s\nGitHub identity: %s (%s)\n", t.OpenShell.Name, t.OpenShell.Identity.Login, t.OpenShell.Repository)
	}
	if t.ParentID != "" {
		fmt.Fprintln(w, "Parent:", t.ParentID)
	}
	if t.Session.BlockID != "" {
		fmt.Fprintln(w, "Block:", t.Session.BlockID)
	}
	if t.Session.ConversationID != "" {
		fmt.Fprintln(w, "Conversation:", t.Session.ConversationID)
	}
	if t.LastActivityAt != "" {
		fmt.Fprintln(w, "Last activity:", t.LastActivityAt)
	}
	if t.WorkspaceRemovedAt != "" {
		fmt.Fprintln(w, "Workspace GC checkpoint:", t.WorkspaceCommit)
	}
	if t.Session.ExitCode != nil {
		fmt.Fprintln(w, "Exit code:", *t.Session.ExitCode)
	}
	if t.Error != "" {
		fmt.Fprintln(w, "Error:", t.Error)
	}
	fmt.Fprintf(w, "Prompt: %s\n", t.Prompt)
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(out, help)
		return nil
	}
	if args[0] == "--version" {
		fmt.Fprintln(out, "paddock 0.1.0")
		return nil
	}
	command := args[0]
	if command == "runtime" {
		return runRuntime(args[1:], out)
	}
	if command == "gc" {
		return runGC(args[1:], out)
	}
	if command == "lifecycle" {
		return runLifecycle(args[1:], out)
	}
	if command == "orchestrate" {
		return runOrchestrate(args[1:], out)
	}
	if command == "planner" {
		return runPlanner(args[1:], out)
	}
	if command == "skill" {
		return runSkill(args[1:], out)
	}
	if command == "github" {
		return runGitHub(args[1:], out)
	}
	if command == "repo" {
		return runRepo(args[1:], out)
	}
	allowed := map[string]string{"run": "agent terminal repo base runtime json detach client", "delegate": "agent terminal repo base runtime parent json", "resume": "json detach client", "message": "", "tasks": "json", "show": "json", "attach": "client", "__run": "generation"}
	flags, ok := allowed[command]
	if !ok {
		return fmt.Errorf("unknown command %q; use paddock --help", command)
	}
	o, pos, err := parse(args[1:], flags)
	if err != nil {
		return err
	}
	if o["help"] != "" {
		fmt.Fprint(out, help)
		return nil
	}
	expected := 1
	if command == "message" {
		expected = 2
	}
	if command == "tasks" {
		expected = 0
	}
	if len(pos) != expected {
		return fmt.Errorf("%s expects %d argument(s); quote the task prompt; use paddock --help", command, expected)
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	// Validate before creating registry files, including a misconfigured in-repo home.
	if command == "run" {
		dir := o["repo"]
		if dir == "" {
			dir, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		dir, err = paddock.ResolveRepoArgument(home, dir)
		if err != nil {
			return err
		}
		repo, err := paddock.ResolveRepository(context.Background(), dir, o["base"])
		if err != nil {
			return err
		}
		if err = paddock.ValidateStateHome(home, repo.Root); err != nil {
			return err
		}
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer store.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	service := paddock.Service{Store: store, Executable: exe}
	ctx := context.Background()
	if command == "__run" {
		generation := 0
		if o["generation"] != "" {
			generation, err = strconv.Atoi(o["generation"])
			if err != nil || generation < 0 {
				return fmt.Errorf("invalid supervisor generation")
			}
		}
		code := paddock.RunAgentGeneration(store, pos[0], exe, generation)
		store.Close()
		os.Exit(code)
	}
	switch command {
	case "resume":
		t, err := service.Resume(ctx, pos[0])
		if err != nil {
			return err
		}
		return presentLaunchedTask(ctx, "run", t, o, out, os.Stderr, service.Attach)
	case "run", "delegate":
		parent := o["parent"]
		if command == "delegate" && parent == "" {
			parent = os.Getenv("PADDOCK_TASK_ID")
			if parent == "" {
				return fmt.Errorf("delegate requires --parent <id> or PADDOCK_TASK_ID from a running task")
			}
		}
		t, err := service.Run(ctx, paddock.RunOptions{Prompt: pos[0], Parent: parent, Agent: o["agent"], Backend: o["terminal"], Repo: o["repo"], Base: o["base"], Runtime: o["runtime"]})
		if err != nil {
			return err
		}
		return presentLaunchedTask(ctx, command, t, o, out, os.Stderr, service.Attach)
	case "message":
		t, e := store.Get(pos[0])
		if e != nil {
			return e
		}
		thread, e := service.Message(ctx, t, pos[1])
		if e != nil {
			return e
		}
		fmt.Fprintln(out, "Queued message for agent session", thread)
	case "tasks":
		tasks, err := store.List("")
		if err != nil {
			return err
		}
		type row struct {
			paddock.Task
			Terminal paddock.Observation `json:"terminal"`
		}
		rows := []row{}
		for _, t := range tasks {
			t, obs, e := service.Refresh(ctx, t)
			if e != nil {
				return e
			}
			rows = append(rows, row{t, obs})
		}
		if o["json"] != "" {
			return emit(out, rows)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATUS\tAGENT\tTERMINAL\tPARENT\tPROMPT")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s:%s\t%s\t%s\n", short(r.ID), r.Status, r.Session.Agent, r.Session.Backend, r.Terminal.State, short(r.ParentID), oneLine(r.Prompt))
		}
		return w.Flush()
	case "show":
		t, err := store.Get(pos[0])
		if err != nil {
			return err
		}
		t, obs, err := service.Refresh(ctx, t)
		if err != nil {
			return err
		}
		children, err := store.List(t.ID)
		if err != nil {
			return err
		}
		for i, c := range children {
			children[i], _, err = service.Refresh(ctx, c)
			if err != nil {
				return err
			}
		}
		if o["json"] != "" {
			return emit(out, paddock.Detail{Task: t, Terminal: obs, Children: children})
		}
		printTask(out, t)
		fmt.Fprintln(out, "Terminal state:", obs.State)
		if obs.Error != "" {
			fmt.Fprintln(out, "Inspection error:", obs.Error)
		}
		fmt.Fprintln(out, "Children:")
		for _, c := range children {
			fmt.Fprintf(out, "  %s  %s  %s  %s\n", c.ID, c.Status, c.Session.Agent, oneLine(c.Prompt))
		}
	case "attach":
		t, err := store.Get(pos[0])
		if err != nil {
			return err
		}
		return service.Attach(ctx, t, o["client"])
	}
	return nil
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "paddock:", err)
		os.Exit(1)
	}
}
