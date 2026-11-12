package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

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
	maxPrompt := strings.Repeat("x", maxPromptBytes)
	prompt, err = loadPrompt("run", []string{"-"}, "", strings.NewReader(maxPrompt))
	if err != nil {
		t.Fatalf("rejected stdin prompt of exactly %d bytes: %v", maxPromptBytes, err)
	}
	if prompt != maxPrompt {
		t.Fatalf("stdin prompt changed: got %d bytes, want %d", len(prompt), len(maxPrompt))
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
		{[]string{"first prompt", "second prompt"}, "", ""},
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
	if !strings.Contains(out.String(), "delegate") {
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
