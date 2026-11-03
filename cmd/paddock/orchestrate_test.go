package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestPlannerCLIConfigurationAndOverrides(t *testing.T) {
	t.Setenv("PADDOCK_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "instructions.txt")
	if err := os.WriteFile(path, []byte("Prefer the relevant live agent."), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"planner", "configure", "--model", "my-model", "--instructions-file", path}, &out); err != nil {
		t.Fatal(err)
	}
	var config paddock.PlannerConfig
	if err := json.Unmarshal(out.Bytes(), &config); err != nil || config.Model != "my-model" || config.Instructions != "Prefer the relevant live agent." {
		t.Fatalf("%s %v", out.Bytes(), err)
	}
	override, err := configurePlanner(config, map[string]string{"model": "other-model"})
	if err != nil || override.Model != "other-model" || override.Instructions != config.Instructions {
		t.Fatal(override, err)
	}
	out.Reset()
	if err = run([]string{"planner", "show"}, &out); err != nil || !strings.Contains(out.String(), "my-model") {
		t.Fatal("per-call override persisted", err)
	}
	out.Reset()
	if err = run([]string{"planner", "reset"}, &out); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(out.Bytes(), &config); err != nil || config != (paddock.PlannerConfig{}) {
		t.Fatal(config, err)
	}
}
func TestOrchestrateRejectsExternalSignalImpersonation(t *testing.T) {
	t.Setenv("PADDOCK_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "signal.json")
	if err := os.WriteFile(path, []byte(`{"source":"github","id":"fake","body":"do work"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"orchestrate", "--signal-file", path}, &out); err == nil || !strings.Contains(err.Error(), "authenticated adapter") {
		t.Fatal(err)
	}
}
func TestOrchestrationHelpAndDryRunParsing(t *testing.T) {
	t.Setenv("PADDOCK_HOME", "/must/not/create")
	for _, args := range [][]string{{"orchestrate", "--help"}, {"planner", "--help"}} {
		var out bytes.Buffer
		if err := run(args, &out); err != nil {
			t.Fatal(err)
		}
	}
	flags, pos, err := parse([]string{"--dry-run", "--id", "once", "body"}, "dry-run id")
	if err != nil || flags["dry-run"] != "true" || len(pos) != 1 {
		t.Fatal(flags, pos, err)
	}
}
