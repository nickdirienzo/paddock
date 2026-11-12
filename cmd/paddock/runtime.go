package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const runtimeHelp = `paddock runtime show
paddock runtime check
paddock runtime default [openshell]
paddock runtime configure --binary /path/to/openshell --gateway NAME
  --image REPOSITORY@sha256:DIGEST --policy /path/to/policy.json
  [--workspace default] [--config-home DIR] [--state-home DIR] [--tls-dir DIR]
  [--codex-provider NAME] [--codex-account-id ID] [--claude-provider NAME] [--allow engineer1,engineer2] [--memory 24Gi] [--cpu 8]

Agent tasks run in OpenShell. Existing host tasks remain inspectable but cannot
resume. Configuration contains references, never secrets.
Build the agent image and configure model providers using docs/openshell.md first.
Use --allow '*' for any GitHub user or bot on automatically discovered PR watches.
Paddock's own App identity and marked replies remain excluded.
`

func runRuntime(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprint(out, runtimeHelp)
		return nil
	}
	flags, pos, e := parse(args[1:], "binary gateway workspace config-home state-home tls-dir image policy codex-provider claude-provider memory cpu codex-account-id allow")
	if e != nil {
		return e
	}
	if flags["help"] != "" {
		fmt.Fprint(out, runtimeHelp)
		return nil
	}
	if (args[0] != "default" && len(pos) != 0) || (args[0] == "default" && len(pos) > 1) {
		return fmt.Errorf("unexpected runtime arguments")
	}
	home, e := paddock.StateHome()
	if e != nil {
		return e
	}
	s, e := paddock.OpenStore(home)
	if e != nil {
		return e
	}
	defer s.Close()
	switch args[0] {
	case "default":
		if len(flags) != 0 {
			return fmt.Errorf("runtime default accepts only openshell")
		}
		if len(pos) == 1 {
			if e = s.SetDefaultRuntime(pos[0]); e != nil {
				return e
			}
		}
		value, err := s.DefaultRuntime()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, value)
		return nil
	case "show":
		c, e := s.OpenShellConfig()
		if e != nil {
			return e
		}
		return emit(out, c)
	case "check":
		if e = (paddock.Service{Store: s}).CheckOpenShell(context.Background()); e != nil {
			return e
		}
		fmt.Fprintln(out, "OpenShell gateway reachable; run a task to validate its image, policy, and providers.")
		return nil
	case "configure":
		c, _ := s.OpenShellConfig()
		if c.Workspace == "" {
			c.Workspace = "default"
		}
		if c.Providers == nil {
			c.Providers = map[string]string{}
		}
		for k, d := range map[string]*string{"codex-account-id": &c.CodexAccountID, "binary": &c.Binary, "gateway": &c.Gateway, "workspace": &c.Workspace, "config-home": &c.ConfigHome, "state-home": &c.StateHome, "tls-dir": &c.TLSDir, "image": &c.Image, "policy": &c.Policy, "memory": &c.Memory, "cpu": &c.CPU} {
			if v, ok := flags[k]; ok {
				*d = v
			}
		}
		if c.Binary == "" {
			c.Binary = "openshell"
		}
		c.Binary, e = exec.LookPath(c.Binary)
		if e != nil {
			return fmt.Errorf("install OpenShell 0.1.6 and pass --binary: %w", e)
		}
		c.Binary, e = filepath.Abs(c.Binary)
		if e != nil {
			return e
		}
		for _, p := range []*string{&c.Policy, &c.ConfigHome, &c.StateHome, &c.TLSDir} {
			if *p != "" {
				*p, e = filepath.Abs(*p)
				if e != nil {
					return e
				}
			}
		}
		for _, agent := range []string{"codex", "claude"} {
			if v := flags[agent+"-provider"]; v != "" {
				c.Providers[agent] = v
			}
		}
		if v := flags["allow"]; v != "" {
			c.WatchAuthors = strings.Split(v, ",")
		}
		if e = s.SetOpenShellConfig(c); e != nil {
			return e
		}
		return emit(out, c)
	default:
		return fmt.Errorf("unknown runtime command; use paddock runtime --help")
	}
}
