package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const orchestrationHelp = `Ephemeral orchestration:
  paddock orchestrate --id <stable-id> [--task <id>] [--tasks id,id] [--repos alias,path]
                      [--model <model>] [--instructions-file <path>] [--dry-run] [--json] "signal text"
  paddock orchestrate --signal-file <json-file> [scope/model/dry-run options]
  paddock orchestrate show <source> <id>
  paddock planner configure [--model <model>] [--instructions-file <path>]
  paddock planner show
  paddock planner reset

Signals use {"source":"cli","id":"stable-id","body":"intent","task_id":"optional-origin"}.
The local CLI accepts source=cli; source adapters authorize external signals separately.
Origin task is included in scope. --tasks and --repos explicitly expand that scope.
Dry runs persist a preview without consuming the signal or executing actions.
Planning uses the existing Codex subscription login, never direct API keys.
`

func orchestrationService() (paddock.Service, func(), error) {
	home, err := paddock.StateHome()
	if err != nil {
		return paddock.Service{}, nil, err
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return paddock.Service{}, nil, err
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		store.Close()
		return paddock.Service{}, nil, err
	}
	return paddock.Service{Store: store, Executable: exe}, func() { store.Close() }, nil
}
func csv(s string) []string {
	result := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			result = append(result, v)
		}
	}
	return result
}
func configurePlanner(config paddock.PlannerConfig, flags map[string]string) (paddock.PlannerConfig, error) {
	if model, ok := flags["model"]; ok {
		config.Model = model
	}
	if path, ok := flags["instructions-file"]; ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return config, err
		}
		if len(data) > 16000 {
			return config, fmt.Errorf("planner instructions must be at most 16KB")
		}
		config.Instructions = string(data)
	}
	return config, nil
}
func runPlanner(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprint(out, orchestrationHelp)
		return nil
	}
	flags, pos, err := parse(args[1:], "model instructions-file json")
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected planner arguments")
	}
	s, close, err := orchestrationService()
	if err != nil {
		return err
	}
	defer close()
	config, err := s.Store.PlannerConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "configure":
		config, err = configurePlanner(config, flags)
		if err == nil {
			err = s.Store.SetPlannerConfig(config)
		}
	case "reset":
		config = paddock.PlannerConfig{}
		err = s.Store.SetPlannerConfig(config)
	case "show":
	default:
		return fmt.Errorf("use planner configure, show, or reset")
	}
	if err != nil {
		return err
	}
	return emit(out, config)
}
func runOrchestrate(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprint(out, orchestrationHelp)
		return nil
	}
	s, close, err := orchestrationService()
	if err != nil {
		return err
	}
	defer close()
	if args[0] == "show" {
		if len(args) != 3 {
			return fmt.Errorf("orchestrate show expects source and ID")
		}
		record, err := s.Store.SignalRecord(args[1], args[2])
		if err != nil {
			return err
		}
		return emit(out, record)
	}
	flags, pos, err := parse(args, "id task tasks repos model instructions-file dry-run json signal-file")
	if err != nil {
		return err
	}
	if flags["help"] != "" {
		fmt.Fprint(out, orchestrationHelp)
		return nil
	}
	signal := paddock.Signal{Source: "cli", ID: flags["id"], TaskID: flags["task"]}
	if path := flags["signal-file"]; path != "" {
		if len(pos) > 0 || flags["id"] != "" || flags["task"] != "" {
			return fmt.Errorf("signal-file replaces id, task, and body")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		decoder := json.NewDecoder(io.LimitReader(file, 128000))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&signal); err != nil {
			return err
		}
		if decoder.Decode(new(any)) != io.EOF {
			return fmt.Errorf("trailing signal data")
		}
	} else {
		if len(pos) != 1 {
			return fmt.Errorf("provide one signal body or --signal-file")
		}
		signal.Body = pos[0]
	}
	if signal.Source != "cli" {
		return fmt.Errorf("external signals must enter through their authenticated adapter")
	}
	if signal.TaskID != "" {
		t, err := s.Store.Get(signal.TaskID)
		if err != nil {
			return err
		}
		signal.TaskID = t.ID
	}
	ctx, cancel := signalContext()
	defer cancel()
	policy, err := s.ResolveOrchestrationPolicy(ctx, signal.TaskID, csv(flags["tasks"]), csv(flags["repos"]))
	if err != nil {
		return err
	}
	config, err := s.Store.PlannerConfig()
	if err == nil {
		config, err = configurePlanner(config, flags)
	}
	if err != nil {
		return err
	}
	record, processErr := (paddock.Orchestrator{Service: s, Planner: paddock.CodexPlanner{Model: config.Model, Instructions: config.Instructions}}).Process(ctx, signal, paddock.OrchestrationOptions{DryRun: flags["dry-run"] != "", Policy: policy})
	if flags["json"] != "" {
		if err = emit(out, record); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "Signal: %s/%s\nStatus: %s\nPlanner: %s\n", signal.Source, signal.ID, record.Status, record.Planner)
		for _, result := range record.Outcomes {
			fmt.Fprintf(out, "%s [%s]: %s\n", result.Action.Kind, result.Status, result.Action.Reason)
			if result.Action.TaskID != "" {
				fmt.Fprintln(out, "  Target:", result.Action.TaskID)
			}
			if result.Action.Repo != "" {
				fmt.Fprintln(out, "  Repository:", result.Action.Repo)
			}
			if result.Action.Text != "" {
				fmt.Fprintln(out, "  Text:", result.Action.Text)
			}
			if result.Result != "" {
				fmt.Fprintln(out, "  Result:", result.Result)
			}
			if result.Error != "" {
				fmt.Fprintln(out, "  Error:", result.Error)
			}
		}
	}
	return processErr
}
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	return ctx, func() { cancel(); stop() }
}
