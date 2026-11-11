package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const tpHelp = `Usage: paddock tp [--agent codex|claude] [--terminal auto|rex|screen] [--json] [--detach] [--client Rex-client-id]

Open the persistent Team Principal conversation for this Paddock registry.
The team is every agent working on Paddock tasks, across all projects.
First launch defaults to Codex; later invocations reconnect or resume the same task.
TP runs on the host to use Paddock's CLI; delegated work keeps the configured runtime.
Project decisions live in projects/ inside TP's retained worktree.
--json and --detach open the task in the background without attaching.
`

func runTP(args []string, out io.Writer) error {
	o, pos, err := parse(args, "agent terminal json detach client")
	if err != nil {
		return err
	}
	if o["help"] != "" {
		_, err = fmt.Fprint(out, tpHelp)
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("tp expects no arguments; use paddock tp --help")
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
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	service := paddock.Service{Store: store, Executable: exe}
	ctx := context.Background()
	task, err := service.TeamPrincipal(ctx, paddock.TPOptions{Agent: o["agent"], Backend: o["terminal"]})
	if err != nil {
		return err
	}
	if o["json"] != "" {
		return emit(out, task)
	}
	fmt.Fprintf(out, "Team Principal: %s (%s)\nProject context: %s\n", task.ID, task.Session.Agent, filepath.Join(task.Worktree, "projects"))
	if o["detach"] != "" {
		fmt.Fprintln(out, "Return: paddock tp")
		return nil
	}
	if err = service.Attach(ctx, task, o["client"]); err != nil {
		return fmt.Errorf("Team Principal task %s is retained but could not be displayed: %w; use paddock tp to reconnect", task.ID, err)
	}
	return nil
}
