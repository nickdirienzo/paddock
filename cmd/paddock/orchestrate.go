package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const orchestrationHelp = `Model-backed orchestration is unavailable until its planner runs in OpenShell.
  paddock orchestrate show <source> <id>
  paddock planner configure [--model <model>] [--instructions-file <path>]
  paddock planner show
  paddock planner reset
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
	if args[0] != "show" {
		return fmt.Errorf("model-backed orchestration requires OpenShell and is currently unavailable; use run and message for deterministic dispatch")
	}
	s, close, err := orchestrationService()
	if err != nil {
		return err
	}
	defer close()
	if len(args) != 3 {
		return fmt.Errorf("orchestrate show expects source and ID")
	}
	record, err := s.Store.SignalRecord(args[1], args[2])
	if err != nil {
		return err
	}
	return emit(out, record)
}
