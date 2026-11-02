package main

import (
	"bytes"
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
