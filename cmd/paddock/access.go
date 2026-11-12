package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const accessHelp = `paddock access pending [task] [--json]
paddock access show REQUEST [--json]
paddock access approve REQUEST [--json]
paddock access reject REQUEST --reason TEXT [--json]

Access requests mirror OpenShell network-rule proposals. Approval applies the
exact reviewed proposal to that task's sandbox and then notifies its agent.
`

func printAccess(w io.Writer, request paddock.AccessRequest) {
	fmt.Fprintf(w, "Request: %s\nTask: %s\nSandbox: %s\nStatus: %s\n", request.ID, request.TaskID, request.Sandbox, request.Status)
	if request.Rule != "" {
		fmt.Fprintln(w, "Rule:", request.Rule)
	}
	if request.Binary != "" {
		fmt.Fprintln(w, "Binary:", request.Binary)
	}
	if request.Endpoints != "" {
		fmt.Fprintln(w, "Endpoints:", request.Endpoints)
	}
	if request.Rationale != "" {
		fmt.Fprintln(w, "Rationale:", request.Rationale)
	}
	if request.Prover != "" {
		fmt.Fprintln(w, "Prover:", request.Prover)
	}
	for _, finding := range request.Security {
		fmt.Fprintln(w, "Security:", finding)
	}
	if request.ApplicationError != "" {
		fmt.Fprintln(w, "Application error:", request.ApplicationError)
	}
	if request.Hits != "" {
		fmt.Fprintln(w, "Hits:", request.Hits)
	}
	if request.Decision != "" {
		fmt.Fprintln(w, "Decision:", request.Decision)
	}
	if request.DecisionReason != "" {
		fmt.Fprintln(w, "Decision reason:", request.DecisionReason)
	}
	if request.DecisionError != "" {
		fmt.Fprintln(w, "Decision error:", request.DecisionError)
	}
	if request.AgentNotificationError != "" {
		fmt.Fprintln(w, "Agent notification error:", request.AgentNotificationError)
	}
}

func runAccess(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(out, accessHelp)
		return nil
	}
	command := args[0]
	allowed := map[string]string{"pending": "json", "show": "json", "approve": "json", "reject": "reason json"}
	flags, ok := allowed[command]
	if !ok {
		return fmt.Errorf("unknown access command %q; use paddock access --help", command)
	}
	opts, pos, err := parse(args[1:], flags)
	if err != nil {
		return err
	}
	if opts["help"] != "" {
		fmt.Fprint(out, accessHelp)
		return nil
	}
	expected := 1
	if command == "pending" {
		expected = -1
		if len(pos) > 1 {
			return fmt.Errorf("access pending accepts at most one task ID")
		}
	} else if len(pos) != expected {
		return fmt.Errorf("access %s expects one request ID", command)
	}
	if command == "reject" && opts["reason"] == "" {
		return fmt.Errorf("access reject requires --reason TEXT")
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

	if command == "pending" {
		taskID := ""
		if len(pos) == 1 {
			task, getErr := store.Get(pos[0])
			if getErr != nil {
				return getErr
			}
			taskID = task.ID
			if _, err = service.SyncAccess(ctx, task); err != nil {
				return err
			}
		} else if err = service.SyncRunningAccess(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "paddock: some access requests could not be refreshed:", err)
		}
		requests, listErr := store.ListAccess(taskID, "pending")
		if listErr != nil {
			return listErr
		}
		if opts["json"] != "" {
			return emit(out, requests)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTASK\tENDPOINTS\tBINARY\tRATIONALE")
		for _, request := range requests {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", short(request.ID), short(request.TaskID), request.Endpoints, request.Binary, oneLine(request.Rationale))
		}
		return w.Flush()
	}

	request, err := store.GetAccess(pos[0])
	if err != nil {
		return err
	}
	if command == "show" {
		if task, taskErr := store.Get(request.TaskID); taskErr == nil {
			_, _ = service.SyncAccess(ctx, task)
			request, _ = store.GetAccess(request.ID)
		}
		if opts["json"] != "" {
			return emit(out, request)
		}
		printAccess(out, request)
		return nil
	}
	request, err = service.DecideAccess(ctx, request.ID, command, opts["reason"])
	if err != nil {
		return err
	}
	if opts["json"] != "" {
		return emit(out, request)
	}
	printAccess(out, request)
	if request.AgentNotificationError != "" {
		fmt.Fprintf(out, "Access was %s, but automatic agent notification failed. Use paddock message %s to continue it.\n", request.Status, request.TaskID)
	}
	return nil
}
