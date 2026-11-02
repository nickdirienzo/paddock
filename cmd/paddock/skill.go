package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const skillHelp = `Paddock agent skill (per user, across repositories):
  paddock skill setup [--agent codex|claude|all]
  paddock skill status [--agent codex|claude|all] [--json]
  paddock skill remove [--agent codex|claude|all]
  paddock skill show

Default: both Codex (~/.agents/skills) and Claude Code (~/.claude/skills).
Setup writes only Paddock's skill; it does not change repository files or agent hooks.
Start a new agent session after setup if the skill is not discovered immediately.
`

func runSkill(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprint(out, skillHelp)
		return err
	}
	command := args[0]
	allowed, ok := map[string]string{"setup": "agent", "remove": "agent", "status": "agent json", "show": ""}[command]
	if !ok {
		return fmt.Errorf("unknown skill command; use paddock skill --help")
	}
	flags, pos, err := parse(args[1:], allowed)
	if err != nil {
		return err
	}
	if flags["help"] != "" {
		_, err = fmt.Fprint(out, skillHelp)
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("skill %s expects no positional arguments", command)
	}
	if command == "show" {
		_, err = fmt.Fprint(out, paddock.AgentGuide)
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var entries []paddock.SkillInstallation
	if command == "status" {
		entries, err = paddock.SkillInstallations(home, flags["agent"])
	} else {
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		exe, e = filepath.EvalSymlinks(exe)
		if e != nil {
			return e
		}
		entries, err = paddock.ConfigureSkills(home, flags["agent"], exe, command == "remove")
	}
	if err != nil {
		return err
	}
	if flags["json"] != "" {
		return emit(out, entries)
	}
	for _, entry := range entries {
		fmt.Fprintf(out, "%s: %s (%s)\n", entry.Agent, entry.State, entry.Path)
	}
	return nil
}
