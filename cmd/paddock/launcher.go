package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const launcherHelp = `Launcher configurations:
  paddock launcher list [--json]
  paddock launcher show NAME [--json]
  paddock launcher configure NAME [--runtime host|openshell] [--agent codex|claude]
    [--terminal auto|rex|screen] [--codex-tui default|scrollback|fullscreen]
    [--read-only true|false] [--file config.json]

Identities select a named launcher with identity create|configure --launcher NAME.
Run flags override launcher settings. Without a mapping, the default launcher
uses the runtime preference (OpenShell when unset), Codex, and automatic terminal
selection. A config file can also supply an openshell configuration object.
`

func runLauncher(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprint(out, launcherHelp)
		return err
	}
	allowed, ok := map[string]string{"list": "json", "show": "json", "configure": "runtime agent terminal codex-tui read-only file"}[args[0]]
	if !ok {
		return fmt.Errorf("unknown launcher command; use paddock launcher --help")
	}
	flags, pos, err := parse(args[1:], allowed)
	if err != nil {
		return err
	}
	if flags["help"] != "" {
		_, err = fmt.Fprint(out, launcherHelp)
		return err
	}
	if (args[0] == "list" && len(pos) != 0) || (args[0] != "list" && len(pos) != 1) {
		return fmt.Errorf("use paddock launcher %s %s", args[0], map[bool]string{true: "", false: "NAME"}[args[0] == "list"])
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	s, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer s.Close()
	configs, err := s.LauncherConfigs()
	if err != nil {
		return err
	}
	if args[0] == "list" {
		return emit(out, configs)
	}
	c, exists := configs[pos[0]]
	if args[0] == "show" {
		if !exists {
			return fmt.Errorf("unknown launcher %q", pos[0])
		}
		return emit(out, c)
	}
	if path := flags["file"]; path != "" {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		c = paddock.LauncherConfig{}
		if err = decoder.Decode(&c); err != nil {
			return fmt.Errorf("launcher config: %w", err)
		}
	}
	for key, target := range map[string]*string{"runtime": &c.Runtime, "agent": &c.Agent, "terminal": &c.Terminal, "codex-tui": &c.CodexTUI} {
		if v, ok := flags[key]; ok {
			*target = v
		}
	}
	if v, ok := flags["read-only"]; ok {
		c.ReadOnly, err = strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("--read-only must be true or false")
		}
	}
	if err = s.SetLauncherConfig(pos[0], c); err != nil {
		return err
	}
	return emit(out, c)
}
