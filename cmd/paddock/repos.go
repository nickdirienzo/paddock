package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func runRepo(args []string, out io.Writer) error {
	opts, pos, err := parse(args, "json")
	if err != nil {
		return err
	}
	if len(pos) == 0 || opts["help"] != "" {
		fmt.Fprint(out, "Usage: paddock repo add <name> <path> | list [--json] | remove <name> | trust <name-or-path> | untrust <name-or-path>\n")
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
		path, err := store.SetRepoTrust(context.Background(), pos[1], trusted)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Codex project trust for future tasks: %t (%s)\n", trusted, path)
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
