package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func runLifecycle(args []string, out io.Writer) error {
	const usage = "paddock lifecycle show\npaddock lifecycle configure [--worktree-ttl 168h] [--disposable-directories node_modules,.next]\n"
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprint(out, usage)
		return nil
	}
	opts, pos, err := parse(args[1:], "worktree-ttl disposable-directories")
	if err != nil {
		return err
	}
	if opts["help"] != "" {
		fmt.Fprint(out, usage)
		return nil
	}
	if len(pos) != 0 {
		return fmt.Errorf("unexpected lifecycle arguments")
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer store.Close()
	c, err := store.LifecycleConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "show":
		if len(opts) != 0 {
			return fmt.Errorf("lifecycle show takes no options")
		}
	case "configure":
		if v := opts["worktree-ttl"]; v != "" {
			c.WorktreeTTL = v
		}
		if v := opts["disposable-directories"]; v != "" {
			c.DisposableDirectories = nil
			if v != "none" {
				c.DisposableDirectories = strings.Split(v, ",")
			}
		}
		if err = store.SetLifecycleConfig(c); err != nil {
			return err
		}
	default:
		return fmt.Errorf("use lifecycle show or configure")
	}
	return emit(out, c)
}

func runGC(args []string, out io.Writer) error {
	opts, pos, err := parse(args, "apply json")
	if err != nil {
		return err
	}
	if opts["help"] != "" {
		fmt.Fprintln(out, "paddock gc [--apply] [--json] — preview or remove expired, safe worktrees")
		return nil
	}
	if len(pos) != 0 {
		return fmt.Errorf("gc takes no positional arguments")
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer store.Close()
	results, err := (paddock.Service{Store: store}).GC(context.Background(), opts["apply"] != "")
	if err != nil {
		return err
	}
	if opts["json"] != "" {
		return emit(out, results)
	}
	for _, r := range results {
		action := "keep"
		if r.Eligible {
			action = "eligible"
		}
		if r.Removed {
			action = "removed"
		}
		fmt.Fprintf(out, "%s  %s  %s\n", short(r.TaskID), action, r.Reason)
	}
	return nil
}
