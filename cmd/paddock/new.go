package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
	"github.com/nickdirienzo/paddock/internal/paddock"
	"golang.org/x/term"
)

const newHelp = `Usage: paddock new [--repo name-or-path] [--agent codex|claude]
                   [--runtime openshell] [--base ref] [--terminal auto|rex|screen]
                   [--client Rex-client-id]

Choose a repository and agent, then write a multiline task description.
Use Tab / Shift-Tab to move between fields, Enter for a new line in the
description, and Escape or Ctrl-C to cancel. Agent execution uses OpenShell.
Install a floating Rex launcher with: paddock rex setup
`

type newTaskDraft struct {
	Repo, Path, Agent, Runtime, Prompt string
	Start                              bool
}

type repositoryChoice struct{ Label, Path string }

// Aliases are stable across directories; include the current project without
// requiring registration, and always allow an arbitrary checkout or bare repo.
func newRepositoryChoices(home, cwd, requested, base string) ([]repositoryChoice, string, error) {
	aliases, err := paddock.ReadRepoAliases(home)
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	choices := make([]repositoryChoice, 0, len(names)+2)
	for _, name := range names {
		choices = append(choices, repositoryChoice{name + " · " + aliases[name], aliases[name]})
	}
	selected := ""
	path := cwd
	if requested != "" {
		path, err = paddock.ResolveRepoArgument(home, expandRepositoryPath(requested))
		if err != nil {
			return nil, "", err
		}
	}
	repo, repoErr := paddock.ResolveRepository(context.Background(), path, base)
	if requested != "" && repoErr != nil {
		return nil, "", repoErr
	}
	if repoErr == nil {
		selected = repo.Root
		found := false
		for _, choice := range choices {
			found = found || choice.Path == selected
		}
		if !found {
			choices = append([]repositoryChoice{{"Current project · " + repo.Root, repo.Root}}, choices...)
		}
	}
	if selected == "" && len(choices) > 0 {
		selected = choices[0].Path
	}
	choices = append(choices, repositoryChoice{"Other repository…", ""})
	return choices, selected, nil
}

func expandRepositoryPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func validateNewPrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("describe the task before starting it")
	}
	if len(prompt) > maxPromptBytes {
		return fmt.Errorf("task prompt exceeds %d bytes", maxPromptBytes)
	}
	return nil
}

func newTaskRuntime(home, requested string) (string, error) {
	if requested != "" {
		if requested != "openshell" {
			return "", fmt.Errorf("agent execution requires OpenShell; host runtime is unavailable")
		}
		return requested, nil
	}
	// Read the preference without creating registry files just to show a form.
	return (&paddock.Store{Home: home}).DefaultRuntime()
}

func launcherKeyMap() *huh.KeyMap {
	keys := huh.NewDefaultKeyMap()
	keys.Quit = key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "cancel"))
	keys.Text.NewLine = key.NewBinding(key.WithKeys("enter", "ctrl+j"), key.WithHelp("enter", "new line"))
	keys.Text.Next = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next"))
	return keys
}

func launcherNotice(title, message string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewNote().Title(title).Description(message).Next(true).NextLabel("Close"),
	)).WithKeyMap(launcherKeyMap())
}

func newTaskForm(draft *newTaskDraft, choices []repositoryChoice, home, base string) *huh.Form {
	options := make([]huh.Option[string], 0, len(choices))
	for _, choice := range choices {
		options = append(options, huh.NewOption(choice.Label, choice.Path))
	}
	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("Repository").Options(options...).Height(5).Value(&draft.Repo),
		).Title("New Paddock Task").Description("Choose a repository. Escape cancels."),
		huh.NewGroup(
			huh.NewInput().Title("Repository path or registered name").Placeholder("~/code/my-project").Value(&draft.Path).
				Validate(func(path string) error {
					if strings.TrimSpace(path) == "" {
						return fmt.Errorf("choose a repository")
					}
					path, err := paddock.ResolveRepoArgument(home, expandRepositoryPath(path))
					if err != nil {
						return err
					}
					_, err = paddock.ResolveRepository(context.Background(), path, base)
					return err
				}),
		).Title("New Paddock Task").WithHideFunc(func() bool { return draft.Repo != "" }),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Agent").Options(
				huh.NewOption("Codex", "codex"), huh.NewOption("Claude Code", "claude"),
			).Value(&draft.Agent),
		).Title("Agent").Description("Use ↑/↓ to choose and Tab to move between fields. Escape cancels."),
		huh.NewGroup(
			huh.NewText().Title("What should the agent do?").Placeholder("Describe the task…").
				Lines(8).CharLimit(maxPromptBytes).ExternalEditor(false).ShowLineNumbers(false).
				Value(&draft.Prompt).Validate(validateNewPrompt),
			huh.NewConfirm().Title("Ready?").Affirmative("Start task").Negative("Cancel").Value(&draft.Start),
		).Title("New Paddock Task").Description("Enter adds a line. Tab moves to Start task. Escape cancels."),
	).WithKeyMap(launcherKeyMap())
}

// Reuse run's validation and launch path, with JSON keeping creation separate
// from displaying the session. An attach failure must never create a second task.
func newTaskRunArgs(draft newTaskDraft, flags map[string]string) []string {
	args := []string{"run", "--json", "--repo", draft.Repo, "--agent", draft.Agent, "--runtime", draft.Runtime}
	for _, name := range []string{"base", "terminal"} {
		if value := flags[name]; value != "" {
			args = append(args, "--"+name, value)
		}
	}
	return append(args, "--", draft.Prompt)
}

func runNew(args []string, out io.Writer) error {
	flags, pos, err := parse(args, "repo agent runtime base terminal client")
	if err != nil {
		return err
	}
	if flags["help"] != "" {
		fmt.Fprint(out, newHelp)
		return nil
	}
	if len(pos) != 0 {
		return fmt.Errorf("new takes no positional arguments; use paddock new --help")
	}
	for name, allowed := range map[string]string{"agent": "codex claude", "runtime": "openshell", "terminal": "auto rex screen"} {
		if value := flags[name]; value != "" && !strings.Contains(" "+allowed+" ", " "+value+" ") {
			return fmt.Errorf("invalid --%s %q (use %s)", name, value, allowed)
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("new needs an interactive terminal; use paddock run for scripts")
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	choices, selected, err := newRepositoryChoices(home, cwd, flags["repo"], flags["base"])
	if err != nil {
		return err
	}
	runtime, err := newTaskRuntime(home, flags["runtime"])
	if err != nil {
		return err
	}
	draft := newTaskDraft{Repo: selected, Agent: flags["agent"], Runtime: runtime, Start: true}
	if draft.Agent == "" {
		draft.Agent = "codex"
	}
	if err = newTaskForm(&draft, choices, home, flags["base"]).Run(); errors.Is(err, huh.ErrUserAborted) {
		return nil
	} else if err != nil {
		return err
	}
	if !draft.Start {
		return nil
	}
	if draft.Repo == "" {
		draft.Repo = expandRepositoryPath(draft.Path)
	}
	// Keep errors visible in a floating pane until the user dismisses them.
	var result bytes.Buffer
	fmt.Fprintln(out, "Starting task…")
	if err = run(newTaskRunArgs(draft, flags), &result); err != nil {
		_ = launcherNotice("Could not start task", err.Error()).Run()
		return err
	}
	var task paddock.Task
	if err = json.Unmarshal(result.Bytes(), &task); err != nil {
		return fmt.Errorf("read created task: %w", err)
	}
	printTask(out, task)
	if task.Session.Backend == "rex" {
		if err = (paddock.Service{}).Attach(context.Background(), task, flags["client"]); err != nil {
			message := fmt.Sprintf("Task %s was created.\n\n%s\n\nOpen it with: paddock attach %s", task.ID, err, task.ID)
			_ = launcherNotice("Task started; could not display session", message).Run()
		}
	}
	return nil
}
