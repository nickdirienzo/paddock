package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func runRepo(args []string, out io.Writer) error {
	opts, pos, err := parse(args, "json mcp")
	if err != nil {
		return err
	}
	if len(pos) == 0 || opts["help"] != "" {
		fmt.Fprint(out, "Usage: paddock repo add <name> <path> | list [--json] | remove <name> | trust <name-or-path> [--mcp name,name] | untrust <name-or-path> [--mcp name,name]\n\n--mcp grants or revokes named host Codex HTTPS MCP connections for OpenShell tasks.\nTrust grants access to all tools enabled for those servers using the host login and network.\nGrants are additive; untrust without --mcp revokes project trust and all MCP grants.\nRestart or resume the task to load newly trusted MCPs; revocation blocks new bridge requests immediately.\n")
		return nil
	}
	counts := map[string]int{"add": 3, "list": 1, "remove": 2, "trust": 2, "untrust": 2}
	count, ok := counts[pos[0]]
	if !ok || len(pos) != count {
		return fmt.Errorf("use paddock repo add, list, remove, trust, or untrust; see paddock repo --help")
	}
	if opts["json"] != "" && pos[0] != "list" {
		return fmt.Errorf("--json is supported by repo list")
	}
	if opts["mcp"] != "" && pos[0] != "trust" && pos[0] != "untrust" {
		return fmt.Errorf("--mcp is supported by repo trust and repo untrust")
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	if pos[0] == "list" {
		aliases, err := paddock.ReadRepoAliases(home)
		if err != nil {
			return err
		}
		if opts["json"] != "" {
			return emit(out, aliases)
		}
		names := make([]string, 0, len(aliases))
		for name := range aliases {
			names = append(names, name)
		}
		sort.Strings(names)
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tPATH")
		for _, name := range names {
			fmt.Fprintf(w, "%s\t%s\n", name, aliases[name])
		}
		return w.Flush()
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer store.Close()
	if pos[0] == "trust" || pos[0] == "untrust" {
		trusted := pos[0] == "trust"
		if value := opts["mcp"]; value != "" {
			names := strings.Split(value, ",")
			for i := range names {
				names[i] = strings.TrimSpace(names[i])
			}
			path, err := store.SetRepoMCPTrust(context.Background(), pos[1], names, trusted)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Host MCP trust: %t (%s): %s\n", trusted, path, strings.Join(names, ", "))
			return nil
		}
		path, err := store.SetRepoTrust(context.Background(), pos[1], trusted)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Project trust for future tasks: %t (%s)\n", trusted, path)
		return nil
	}
	if pos[0] == "add" {
		path, err := store.AddRepo(context.Background(), pos[1], pos[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s -> %s\n", pos[1], path)
		return nil
	}
	if err = store.RemoveRepo(pos[1]); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed repo alias %s.\n", pos[1])
	return nil
}
