package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestNewRepositoryChoices(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "--initial-branch=main", repo}, {"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		if b, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root, err := store.AddRepo(context.Background(), "project", repo)
	if err != nil {
		t.Fatal(err)
	}
	choices, selected, err := newRepositoryChoices(home, repo, "", "")
	if err != nil || len(choices) != 2 || selected != root || choices[0].Path != root || choices[1].Path != "" {
		t.Fatal(choices, selected, err)
	}
	choices, selected, err = newRepositoryChoices(home, t.TempDir(), "project", "")
	if err != nil || selected != root || len(choices) != 2 {
		t.Fatal(choices, selected, err)
	}
	if _, _, err = newRepositoryChoices(home, repo, filepath.Join(repo, "missing"), ""); err == nil {
		t.Fatal("ignored invalid explicit repository")
	}
	emptyHome := filepath.Join(t.TempDir(), "uncreated")
	choices, selected, err = newRepositoryChoices(emptyHome, repo, "", "")
	if err != nil || len(choices) != 2 || selected != root || choices[0].Path != root {
		t.Fatal(choices, selected, err)
	}
	choices, selected, err = newRepositoryChoices(emptyHome, t.TempDir(), "", "")
	if err != nil || len(choices) != 1 || selected != "" {
		t.Fatal(choices, selected, err)
	}
	if _, err = os.Stat(emptyHome); !os.IsNotExist(err) {
		t.Fatal("browsing repositories created task state", err)
	}
}

func TestNewTaskPreservesLiteralPromptAndSelectedRuntime(t *testing.T) {
	draft := newTaskDraft{Repo: "/repo with spaces", Agent: "claude", Runtime: "host", Prompt: "--literal $(touch /tmp/nope)\n\nsecond line; `quoted`"}
	args := newTaskRunArgs(draft, map[string]string{"terminal": "rex", "base": "main"})
	flags, pos, err := parse(args[1:], "json repo agent runtime terminal base")
	if err != nil || flags["repo"] != draft.Repo || flags["agent"] != "claude" || flags["terminal"] != "rex" || flags["base"] != "main" || flags["runtime"] != "host" || len(pos) != 1 || pos[0] != draft.Prompt {
		t.Fatal(flags, pos, err)
	}
	// A choice made in the form supersedes the flag that initially selected it.
	draft.Runtime = "openshell"
	flags, _, err = parse(newTaskRunArgs(draft, map[string]string{"runtime": "host"})[1:], "json repo agent runtime")
	if err != nil || flags["runtime"] != "openshell" {
		t.Fatal(flags, err)
	}
	for _, invalid := range []string{" \n\t", strings.Repeat("x", maxPromptBytes+1), strings.Repeat("é", maxPromptBytes/2+1)} {
		if err := validateNewPrompt(invalid); err == nil {
			t.Fatal("accepted empty or oversized prompt")
		}
	}
}

func TestNewTaskRuntimeStartsAtConfiguredDefault(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	if got, err := newTaskRuntime(home, ""); err != nil || got != "openshell" {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("reading default created state", err)
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "runtime.json")
	body := []byte("{\"default\":\"host\"}\n")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := newTaskRuntime(home, ""); err != nil || got != "host" {
		t.Fatal(got, err)
	}
	if got, err := newTaskRuntime(home, "openshell"); err != nil || got != "openshell" {
		t.Fatal(got, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, body) {
		t.Fatal("changed the saved default", string(got), err)
	}
}

func TestNewFormShowsAndSelectsAgentsAndRuntimes(t *testing.T) {
	draft := newTaskDraft{Repo: "/project", Agent: "codex", Runtime: "openshell", Start: true}
	form := newTaskForm(&draft, []repositoryChoice{{"Project", "/project"}}, t.TempDir(), "")
	form.Init()
	form.NextGroup()
	form.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	view := form.View()
	for _, label := range []string{"Codex", "Claude Code", "OpenShell (sandbox)", "Host (local machine)"} {
		if !strings.Contains(view, label) {
			t.Fatalf("option %q is hidden: %s", label, view)
		}
	}
	form.Update(tea.KeyMsg{Type: tea.KeyDown})
	form.NextField()
	form.Update(tea.KeyMsg{Type: tea.KeyDown})
	if draft.Agent != "claude" || draft.Runtime != "host" {
		t.Fatalf("selection did not reach the draft: %+v", draft)
	}
	flags, _, err := parse(newTaskRunArgs(draft, nil)[1:], "json repo agent runtime")
	if err != nil || flags["agent"] != "claude" || flags["runtime"] != "host" {
		t.Fatal("selected settings were not sent to run", flags, err)
	}
}

func TestEscapeCancelsEveryLauncherPage(t *testing.T) {
	for _, page := range []string{"repository", "custom path", "agent", "runtime", "prompt", "submit", "error notice"} {
		t.Run(page, func(t *testing.T) {
			draft := newTaskDraft{Repo: "/project", Agent: "codex", Runtime: "openshell", Start: true}
			if page == "custom path" {
				draft.Repo = ""
			}
			form := newTaskForm(&draft, []repositoryChoice{{"Project", "/project"}, {"Other repository…", ""}}, t.TempDir(), "")
			form.Init()
			if page != "repository" {
				form.NextGroup()
			}
			if page == "runtime" {
				form.NextField()
			}
			if page == "prompt" || page == "submit" {
				form.NextGroup()
				if page == "submit" {
					form.NextField()
				}
			}
			if page == "error notice" {
				form = launcherNotice("Could not start task", "test error")
				form.Init()
			}
			form.Update(tea.KeyMsg{Type: tea.KeyEsc})
			if form.State != huh.StateAborted {
				t.Fatalf("Escape left the form open: state %v", form.State)
			}
		})
	}
}

func TestLauncherHelpNeedsNoState(t *testing.T) {
	t.Setenv("PADDOCK_HOME", "/this/path/must/not/be/created")
	for _, command := range []string{"new", "rex"} {
		var out bytes.Buffer
		if err := run([]string{command, "--help"}, &out); err != nil || !strings.Contains(out.String(), "Usage:") {
			t.Fatal(command, out.String(), err)
		}
	}
	for _, args := range [][]string{{"new", "unexpected"}, {"new", "--agent", "invalid"}, {"new", "--runtime", "invalid"}, {"new", "--terminal", "invalid"}, {"rex", "invalid"}, {"rex", "remove", "--key", "cmd+p"}} {
		if err := run(args, io.Discard); err == nil {
			t.Fatal("accepted invalid arguments", args)
		}
	}
}
