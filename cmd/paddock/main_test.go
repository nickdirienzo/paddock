package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestPrintTaskDistinguishesLogicalIdentityAndGitHubActor(t *testing.T) {
	task := paddock.Task{Provenance: paddock.TaskProvenance{Caller: "501:engineer", Requester: "user-123", RequesterSource: "service-api", Identity: "investigator", Profile: "investigation", Capabilities: []string{"repository.read", "analysis"}}, OpenShell: &paddock.OpenShellTask{Name: "pd-task"}}
	var output bytes.Buffer
	printTask(&output, task)
	for _, field := range []string{"Local caller: 501:engineer", "Asserted requester: user-123 (service-api)", "Paddock identity: investigator", "Profile: investigation", "Capabilities: repository.read, analysis"} {
		if !strings.Contains(output.String(), field) {
			t.Fatalf("missing %q in %s", field, output.String())
		}
	}
	if strings.Contains(output.String(), "GitHub actor:") {
		t.Fatalf("unbacked identity was displayed as a GitHub actor: %s", output.String())
	}
	task.Provenance.GitHubActor = "helper[bot]"
	task.OpenShell.Repository = "team/project"
	output.Reset()
	printTask(&output, task)
	if !strings.Contains(output.String(), "GitHub actor: helper[bot] (team/project)") {
		t.Fatal(output.String())
	}
}

func TestCLIParsing(t *testing.T) {
	o, p, e := parse([]string{"prompt with ; $(literal)", "--agent", "claude", "--json"}, "agent json")
	if e != nil || len(p) != 1 || o["agent"] != "claude" || o["json"] != "true" {
		t.Fatalf("%v %v %v", o, p, e)
	}
	_, p, e = parse([]string{"--", "--literal prompt"}, "agent")
	if e != nil || len(p) != 1 || p[0] != "--literal prompt" {
		t.Fatal(p, e)
	}
	for _, args := range [][]string{{"--unknown"}, {"--agent"}, {"--agent=codex", "--agent=claude"}} {
		if _, _, e = parse(args, "agent"); e == nil {
			t.Fatal(args)
		}
	}
}

func TestLoadPrompt(t *testing.T) {
	prompt, err := loadPrompt("run", []string{"one line"}, "", strings.NewReader("unused"))
	if err != nil || prompt != "one line" {
		t.Fatal(prompt, err)
	}
	multiline := "Goal\n\n- first\n- second\n"
	prompt, err = loadPrompt("run", []string{"-"}, "", strings.NewReader(multiline))
	if err != nil || prompt != multiline {
		t.Fatal(prompt, err)
	}
	path := t.TempDir() + "/goal.md"
	if err = os.WriteFile(path, []byte(multiline), 0600); err != nil {
		t.Fatal(err)
	}
	prompt, err = loadPrompt("delegate", nil, path, strings.NewReader("unused"))
	if err != nil || prompt != multiline {
		t.Fatal(prompt, err)
	}
	prompt, err = loadPrompt("run", nil, "-", strings.NewReader(multiline))
	if err != nil || prompt != multiline {
		t.Fatal(prompt, err)
	}
	for _, tc := range []struct {
		pos  []string
		file string
		in   string
	}{
		{nil, "", ""},
		{[]string{"prompt"}, path, ""},
		{[]string{"-"}, "", " \n"},
		{[]string{"-"}, "", strings.Repeat("x", maxPromptBytes+1)},
	} {
		if _, err = loadPrompt("run", tc.pos, tc.file, strings.NewReader(tc.in)); err == nil {
			t.Fatalf("accepted invalid prompt source: %+v", tc)
		}
	}
}
func TestHelpDoesNotNeedRepositoryOrState(t *testing.T) {
	t.Setenv("PADDOCK_HOME", "/this/path/must/not/be/created")
	var out bytes.Buffer
	if e := run([]string{"--help"}, &out); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "delegate") || !strings.Contains(out.String(), "access") {
		t.Fatal(out.String())
	}
}

func TestHostPlannerCLIIsUnavailableWithoutStateChanges(t *testing.T) {
	t.Setenv("PADDOCK_HOME", "/this/path/must/not/be/created")
	if err := run([]string{"orchestrate", "--id", "probe", "route this"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "OpenShell") {
		t.Fatal(err)
	}
}

func TestAccessHelpDoesNotNeedState(t *testing.T) {
	t.Setenv("PADDOCK_HOME", "/this/path/must/not/be/created")
	var out bytes.Buffer
	if err := run([]string{"access", "--help"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "access approve") {
		t.Fatal(out.String())
	}
}

func TestSkillGuideDoesNotNeedRepositoryOrState(t *testing.T) {
	t.Setenv("PADDOCK_HOME", "/this/path/must/not/be/created")
	var out bytes.Buffer
	if err := run([]string{"skill", "show"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("empty agent guide")
	}
	for _, args := range [][]string{{"skill", "show", "unexpected"}, {"skill", "setup", "--agent", "invalid"}} {
		if err := run(args, &out); err == nil {
			t.Fatal("accepted invalid skill command", args)
		}
	}
}

func TestIdleTTLCLIContract(t *testing.T) {
	for _, command := range []string{"run", "delegate"} {
		// Invalid TTL reaches validation rather than being an unknown CLI option.
		args := []string{command, "work", "--idle-ttl", "0s"}
		if command == "delegate" {
			args = append(args, "--parent", "missing")
		}
		t.Setenv("PADDOCK_HOME", t.TempDir())
		var output bytes.Buffer
		if err := run(args, &output); err == nil || !strings.Contains(err.Error(), "positive duration") {
			t.Fatal(command, err)
		}
	}
	if !strings.Contains(help, "--idle-ttl") || !strings.Contains(help, "omitted by default") {
		t.Fatal("missing TTL help")
	}
}
