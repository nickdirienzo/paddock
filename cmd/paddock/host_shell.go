package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func runShellSetup(args []string, out io.Writer) error {
	const usage = "paddock shell-setup <id>\n"
	opts, pos, err := parse(args, "")
	if err != nil {
		return err
	}
	if opts["help"] != "" {
		_, err = fmt.Fprint(out, usage)
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("use %s", usage)
	}
	id := pos[0]
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer store.Close()
	task, err := store.Get(id)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	service := paddock.Service{Store: store, Executable: exe}
	if err = service.EnableHostShell(context.Background(), task); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Host shell shortcut ready: ! paddock attach")
	return err
}
