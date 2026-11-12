package main

import (
	"context"
	"encoding/json"
	"errors"
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

  paddock new [--repo name-or-path] [--agent codex|claude]
  paddock rex setup|remove [--config path] [--key shortcut]
  paddock run [<prompt>|-] [--prompt-file path] [--agent codex|claude] [--terminal auto|rex|screen]
                           [--identity name] [--profile investigation|implementation]
                           [--requester id] [--requester-source service] [--external-id id] [--context-file JSON]
                           [--runtime openshell] [--repo path] [--base ref] [--json] [--detach] [--client Rex-client-id]
  paddock delegate [<prompt>|-] [--prompt-file path] [--parent id] [run options]
  paddock message <id> "message" [--external-id id] [--json]
  paddock tasks [--json]
  paddock show <id> [--json]
  paddock artifacts <id> [--json]
  paddock attach [<id>] [--client Rex-client-id]
  paddock shell-setup <id>
  paddock resume <id> [--json] [--detach] [--client Rex-client-id]
  paddock access pending|show|approve|reject
  paddock identity create|list|show|disable|enable|verify
  paddock runtime show|configure|check|default
  paddock gc [--apply] [--json]
  paddock lifecycle show|configure
  paddock github --help
  paddock orchestrate show <source> <id>
  paddock planner configure|show|reset
  paddock skill setup|status|remove|show
  paddock repo add <name> <path>
  paddock repo list [--json]
  paddock repo remove <name>
  paddock repo trust <name-or-path> [--mcp name,name]
  paddock repo untrust <name-or-path> [--mcp name,name]

Delegate inherits the parent from PADDOCK_TASK_ID when --parent is omitted.
Task IDs accept unique prefixes. State: PADDOCK_HOME or ~/.local/state/paddock.
Bare attach opens a host shell tab. Attach with an ID reconnects to that agent.
Run opens its Rex session automatically; --detach or --json keeps it in the background.
--repo accepts a registered name, a working-tree path, or a bare-repository path.
Use - to read a multiline prompt from stdin until EOF, or --prompt-file to read a file.
`

const maxPromptBytes = 1024 * 1024

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

func loadPrompt(command string, pos []string, promptFile string, stdin io.Reader) (string, error) {
	if promptFile != "" && len(pos) != 0 {
		return "", fmt.Errorf("%s accepts either a prompt argument or --prompt-file, not both", command)
	}
	if promptFile == "" {
		if len(pos) != 1 {
			return "", fmt.Errorf("%s expects a prompt argument, '-' for stdin, or --prompt-file <path>", command)
		}
		if pos[0] != "-" {
			if strings.TrimSpace(pos[0]) == "" {
				return "", fmt.Errorf("a non-empty task prompt is required")
			}
			return pos[0], nil
		}
		promptFile = "-"
	}
	reader := stdin
	var file *os.File
	if promptFile != "-" {
		var err error
		file, err = os.Open(promptFile)
		if err != nil {
			return "", fmt.Errorf("read prompt file: %w", err)
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read task prompt: %w", err)
	}
	if len(data) > maxPromptBytes {
		return "", fmt.Errorf("task prompt exceeds %d bytes", maxPromptBytes)
	}
	prompt := string(data)
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("a non-empty task prompt is required")
	}
	return prompt, nil
}

func printTask(w io.Writer, t paddock.Task) {
	fmt.Fprintf(w, "Task: %s\nStatus: %s\nAgent: %s\nRepository: %s\nBase: %s (%s)\nBranch: %s\nWorktree: %s\nTerminal: %s\nSession: %s\n", t.ID, t.Status, t.Session.Agent, t.Repo, t.Base, t.BaseCommit, t.Branch, t.Worktree, t.Session.Backend, t.Session.ID)
	fmt.Fprintln(w, "Runtime:", t.RuntimeName())
	if t.Provenance.Caller != "" {
		fmt.Fprintln(w, "Local caller:", t.Provenance.Caller)
	}
	if t.Provenance.Requester != "" {
		fmt.Fprintf(w, "Asserted requester: %s (%s)\n", t.Provenance.Requester, t.Provenance.RequesterSource)
	}
	if t.Provenance.Identity != "" {
		fmt.Fprintln(w, "Paddock identity:", t.Provenance.Identity)
	}
	if t.Provenance.Profile != "" {
		fmt.Fprintln(w, "Profile:", t.Provenance.Profile)
	}
	if len(t.Provenance.Capabilities) > 0 {
		fmt.Fprintln(w, "Capabilities:", strings.Join(t.Provenance.Capabilities, ", "))
	}
	if t.OpenShell != nil {
		fmt.Fprintln(w, "Sandbox:", t.OpenShell.Name)
		if t.Provenance.GitHubActor != "" {
			fmt.Fprintf(w, "GitHub actor: %s (%s)\n", t.Provenance.GitHubActor, t.OpenShell.Repository)
		}
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
	if command == "new" {
		return runNew(args[1:], out)
	}
	if command == "rex" {
		return runRex(args[1:], out)
	}
	if command == "runtime" {
		return runRuntime(args[1:], out)
	}
	if command == "access" {
		return runAccess(args[1:], out)
	}
	if command == "identity" {
		return runIdentity(args[1:], out)
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
	if command == "shell-setup" {
		return runShellSetup(args[1:], out)
	}
	allowed := map[string]string{"run": "prompt-file agent terminal repo base runtime identity profile requester requester-source external-id context-file json detach client", "delegate": "prompt-file agent terminal repo base runtime parent identity profile requester requester-source external-id context-file json", "resume": "json detach client", "message": "external-id json", "tasks": "json", "show": "json", "artifacts": "json", "attach": "client", "__run": "generation", "__paste": "generation resume-notice"}
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
	if command == "attach" && len(pos) == 0 {
		_, err := (paddock.Service{}).OpenHostShell(context.Background(), nil, o["client"])
		return err
	}
	prompt := ""
	if command == "run" || command == "delegate" {
		prompt, err = loadPrompt(command, pos, o["prompt-file"], os.Stdin)
		if err != nil {
			return err
		}
	} else {
		expected := 1
		if command == "message" {
			expected = 2
		}
		if command == "tasks" {
			expected = 0
		}
		if len(pos) != expected {
			return fmt.Errorf("%s expects %d argument(s); use paddock --help", command, expected)
		}
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
	if command == "__run" || command == "__paste" {
		generation := 0
		if o["generation"] != "" {
			generation, err = strconv.Atoi(o["generation"])
			if err != nil || generation < 0 {
				return fmt.Errorf("invalid supervisor generation")
			}
		}
		var code int
		if command == "__paste" {
			code = paddock.RunPasteAgent(store, pos[0], generation, o["resume-notice"])
		} else {
			code = paddock.RunAgentGeneration(store, pos[0], exe, generation)
		}
		store.Close()
		os.Exit(code)
	}
	switch command {
	case "artifacts":
		t, e := store.Get(pos[0])
		if e != nil {
			return e
		}
		artifacts, e := service.Artifacts(ctx, t)
		if e != nil {
			return e
		}
		return emit(out, artifacts)
	case "resume":
		t, err := service.Resume(ctx, pos[0])
		if err != nil {
			return err
		}
		return presentLaunchedTask(ctx, "run", t, o, out, os.Stderr, service.Attach)
	case "run", "delegate":
		contextFields := map[string]string{}
		if o["context-file"] != "" {
			data, e := os.ReadFile(o["context-file"])
			if e != nil {
				return e
			}
			if len(data) > 65536 {
				return fmt.Errorf("context file exceeds 64 KiB")
			}
			if e = json.Unmarshal(data, &contextFields); e != nil {
				return fmt.Errorf("context file must be a JSON object of strings: %w", e)
			}
		}
		parent := o["parent"]
		if command == "delegate" && parent == "" {
			parent = os.Getenv("PADDOCK_TASK_ID")
			if parent == "" {
				return fmt.Errorf("delegate requires --parent <id> or PADDOCK_TASK_ID from a running task")
			}
		}
		t, err := service.Run(ctx, paddock.RunOptions{Prompt: prompt, Parent: parent, Agent: o["agent"], Backend: o["terminal"], Repo: o["repo"], Base: o["base"], Runtime: o["runtime"], Identity: o["identity"], Profile: o["profile"], Requester: o["requester"], RequesterSource: o["requester-source"], ExternalID: o["external-id"], Context: contextFields})
		if err != nil {
			return err
		}
		return presentLaunchedTask(ctx, command, t, o, out, os.Stderr, service.Attach)
	case "message":
		t, e := store.Get(pos[0])
		if e != nil {
			return e
		}
		var receipt paddock.MessageReceipt
		if o["external-id"] != "" {
			caller, e := paddock.LocalCaller()
			if e != nil {
				return e
			}
			var claimed bool
			receipt, claimed, e = store.ClaimMessage(t.ID, caller, o["external-id"], pos[1])
			if e != nil {
				return e
			}
			if !claimed {
				if o["json"] != "" {
					return emit(out, receipt)
				}
				_, _ = fmt.Fprintln(out, "Message", receipt.ExternalID, "is", receipt.Status)
				return nil
			}
		}
		thread, e := service.Message(ctx, t, pos[1])
		if o["external-id"] != "" {
			status := "delivered"
			message := ""
			if e != nil {
				status = "uncertain"
				if errors.Is(e, paddock.ErrAgentOffline) {
					status = "pending"
				}
				message = e.Error()
			}
			receipt, err = store.FinishMessage(receipt, status, thread, message)
			if err != nil {
				return err
			}
			if o["json"] != "" {
				if err = emit(out, receipt); err != nil {
					return err
				}
			}
		}
		if e != nil {
			return e
		}
		if o["external-id"] != "" {
			if o["json"] == "" {
				_, _ = fmt.Fprintln(out, "Delivered message", receipt.ExternalID, "to", thread)
			}
			return nil
		}
		if o["json"] != "" {
			return emit(out, map[string]string{"task_id": t.ID, "thread_id": thread, "status": "queued"})
		}
		fmt.Fprintln(out, "Queued message for agent session", thread)
	case "tasks":
		tasks, err := store.List("")
		if err != nil {
			return err
		}
		type row struct {
			paddock.Task
			Terminal      paddock.Observation `json:"terminal"`
			PendingAccess int                 `json:"pending_access,omitempty"`
			Blocked       bool                `json:"blocked"`
		}
		rows := []row{}
		for _, t := range tasks {
			t, obs, e := service.Refresh(ctx, t)
			if e != nil {
				return e
			}
			requests, e := store.ListAccess(t.ID, "pending")
			if e != nil {
				return e
			}
			rows = append(rows, row{Task: t, Terminal: obs, PendingAccess: len(requests), Blocked: len(requests) > 0})
		}
		if o["json"] != "" {
			return emit(out, rows)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATUS\tAGENT\tTERMINAL\tPARENT\tPROMPT")
		for _, r := range rows {
			status := r.Status
			if r.PendingAccess > 0 {
				status = fmt.Sprintf("%s (access:%d)", status, r.PendingAccess)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s:%s\t%s\t%s\n", short(r.ID), status, r.Session.Agent, r.Session.Backend, r.Terminal.State, short(r.ParentID), oneLine(r.Prompt))
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
		accessRequests, err := store.ListAccess(t.ID, "pending")
		if err != nil {
			return err
		}
		var lastTurn *paddock.AgentTurn
		if t.RuntimeName() == "openshell" && t.Session.Agent == "codex" && t.OpenShell != nil && t.OpenShell.Ready && obs.State == "running" {
			turn, turnErr := paddock.CodexLastTurn(ctx, t)
			if turnErr != nil {
				turn.Error = turnErr.Error()
			}
			lastTurn = &turn
		}
		if o["json"] != "" {
			return emit(out, paddock.Detail{Task: t, Terminal: obs, LastTurn: lastTurn, Children: children, AccessRequests: accessRequests, Blocked: len(accessRequests) > 0})
		}
		printTask(out, t)
		fmt.Fprintln(out, "Terminal state:", obs.State)
		if lastTurn != nil {
			fmt.Fprintln(out, "Last model turn:", lastTurn.State)
			if lastTurn.Error != "" {
				fmt.Fprintln(out, "Turn inspection error:", lastTurn.Error)
			}
		}
		if obs.Error != "" {
			fmt.Fprintln(out, "Inspection error:", obs.Error)
		}
		fmt.Fprintln(out, "Children:")
		for _, c := range children {
			fmt.Fprintf(out, "  %s  %s  %s  %s\n", c.ID, c.Status, c.Session.Agent, oneLine(c.Prompt))
		}
		fmt.Fprintln(out, "Pending access requests:")
		for _, request := range accessRequests {
			fmt.Fprintf(out, "  %s  %s  %s\n", request.ID, request.Endpoints, request.Binary)
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
