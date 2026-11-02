package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

// Display is a best-effort CLI convenience after the task is durably launched.
// A remote-control failure must not invite retrying run and creating duplicate work.
func presentLaunchedTask(ctx context.Context, command string, task paddock.Task, flags map[string]string, out, warnings io.Writer, attach func(context.Context, paddock.Task, string) error) error {
	if flags["json"] != "" {
		return emit(out, task)
	}
	printTask(out, task)
	fmt.Fprintf(out, "Attach: paddock attach %s\n", task.ID)
	if command != "run" || task.Session.Backend != "rex" || flags["detach"] != "" || os.Getenv("PADDOCK_TASK_ID") != "" {
		return nil
	}
	if err := attach(ctx, task, flags["client"]); err != nil {
		fmt.Fprintf(warnings, "Task created, but could not display its Rex session: %v\nUse paddock attach %s to open it.\n", err, task.ID)
	}
	return nil
}
