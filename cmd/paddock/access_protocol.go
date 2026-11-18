package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func runAccessV1(args []string, out io.Writer) error {
	command := args[0]
	flags := map[string]string{"list": "task status refresh json", "show": "task refresh json", "decide": "task decision-file json", "reconcile": "task json"}[command]
	opts, pos, err := parse(args[1:], flags)
	fail := func(e error, task, request string) error {
		var wire *paddock.AccessError
		if !errors.As(e, &wire) {
			wire = &paddock.AccessError{Code: "invalid_input", Message: e.Error()}
		}
		_ = emit(out, (paddock.AccessDecisionResponse{ProtocolVersion: 1, TaskID: task, RequestID: request, Error: wire}).WithOutcome())
		return wire
	}
	if err != nil {
		return fail(err, "", "")
	}
	if opts["help"] != "" {
		fmt.Fprint(out, accessHelp)
		return nil
	}
	if (command == "list" && len(pos) != 0) || (command != "list" && len(pos) != 1) {
		return fail(fmt.Errorf("invalid access %s arguments", command), opts["task"], "")
	}
	if command != "decide" && opts["task"] == "" {
		return fail(fmt.Errorf("access %s requires --task with a full task ID", command), "", "")
	}
	var input paddock.AccessDecisionInput
	if command == "decide" {
		if opts["decision-file"] == "" {
			return fail(fmt.Errorf("access decide requires --decision-file"), opts["task"], pos[0])
		}
		input, err = paddock.ReadAccessDecisionFile(opts["decision-file"])
		if err != nil {
			return fail(err, input.TaskID, pos[0])
		}
		if opts["task"] != "" && opts["task"] != input.TaskID {
			return fail(fmt.Errorf("--task must match decision-file task_id"), input.TaskID, pos[0])
		}
	}
	if command == "list" && opts["status"] != "" && opts["status"] != "all" && opts["status"] != "pending" {
		return fail(fmt.Errorf("access list --status must be all or pending"), opts["task"], "")
	}
	home, err := paddock.StateHome()
	if err != nil {
		return fail(err, opts["task"], "")
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return fail(&paddock.AccessError{Code: "storage_error", Message: "Cannot open access registry.", Retryable: true}, opts["task"], "")
	}
	defer store.Close()
	exe, _ := os.Executable()
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	service := paddock.Service{Store: store, Executable: exe}
	ctx := context.Background()
	switch command {
	case "list":
		response := service.ListAccessV1(ctx, opts["task"], opts["refresh"] != "")
		if opts["status"] == "pending" {
			filtered := []paddock.AccessView{}
			for _, r := range response.Requests {
				if r.Status == "pending" {
					filtered = append(filtered, r)
				}
			}
			response.Requests = filtered
		}
		if err = emit(out, response); err != nil {
			return err
		}
		if response.Error != nil {
			return response.Error
		}
	case "show":
		response := service.ShowAccessV1(ctx, opts["task"], pos[0], opts["refresh"] != "")
		if err = emit(out, response); err != nil {
			return err
		}
		if response.Error != nil {
			return response.Error
		}
	case "decide", "reconcile":
		var response paddock.AccessDecisionResponse
		if command == "decide" {
			response = service.DecideAccessV1(ctx, pos[0], input)
		} else {
			response = service.ReconcileAccessV1(ctx, opts["task"], pos[0])
		}
		if err = emit(out, response); err != nil {
			return err
		}
		if response.Error != nil {
			return response.Error
		}
	}
	return nil
}
