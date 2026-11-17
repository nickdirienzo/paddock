package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func runLifecycle(args []string, out io.Writer) error {
	const usage = "paddock lifecycle archive|verify-archive|context|mark-shipped TASK_ID\npaddock lifecycle expire TASK_ID [--apply]\npaddock lifecycle show\npaddock lifecycle configure [--worktree-ttl 168h] [--disposable-directories node_modules,.next]\n"
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprint(out, usage)
		return nil
	}
	flags := ""
	switch args[0] {
	case "configure":
		flags = "worktree-ttl disposable-directories"
	case "expire":
		flags = "apply"
	case "archive", "verify-archive", "context", "mark-shipped", "show":
	default:
		return fmt.Errorf("unknown lifecycle command %q", args[0])
	}
	opts, pos, err := parse(args[1:], flags)
	if err != nil {
		return err
	}
	if opts["help"] != "" {
		fmt.Fprint(out, usage)
		return nil
	}
	if args[0] == "expire" {
		if len(pos) != 1 {
			return fmt.Errorf("lifecycle expire requires one task ID")
		}
		home, e := paddock.StateHome()
		if e != nil {
			return e
		}
		store, e := paddock.OpenStore(home)
		if e != nil {
			return e
		}
		defer store.Close()
		result, e := (paddock.Service{Store: store}).ExpireTask(context.Background(), pos[0], opts["apply"] != "")
		if e != nil {
			return e
		}
		return emit(out, result)
	}
	if args[0] == "archive" || args[0] == "verify-archive" || args[0] == "mark-shipped" || args[0] == "context" {
		if len(pos) != 1 || len(opts) != 0 {
			return fmt.Errorf("lifecycle %s requires exactly one task ID", args[0])
		}
		home, e := paddock.StateHome()
		if e != nil {
			return e
		}
		store, e := paddock.OpenStore(home)
		if e != nil {
			return e
		}
		defer store.Close()
		service := paddock.Service{Store: store}
		if args[0] == "context" {
			text, e := service.ArchivedContext(pos[0])
			if e != nil {
				return e
			}
			_, e = fmt.Fprint(out, text)
			return e
		}
		if args[0] == "mark-shipped" {
			return service.MarkShipped(context.Background(), pos[0])
		}
		var a paddock.ConversationArchive
		if args[0] == "archive" {
			a, e = service.ArchiveConversation(context.Background(), pos[0])
		} else {
			a, e = service.VerifyArchive(pos[0])
		}
		if e != nil {
			return e
		}
		a.Transcript = nil
		a.ContextText = ""
		return emit(out, a)
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
