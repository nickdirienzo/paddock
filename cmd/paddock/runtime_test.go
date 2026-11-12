package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestRuntimeDefaultIsExplicitAndPersistent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PADDOCK_HOME", home)
	read := func(want string) {
		t.Helper()
		var out bytes.Buffer
		if err := runRuntime([]string{"default"}, &out); err != nil || out.String() != want+"\n" {
			t.Fatalf("default: %q %v", out.String(), err)
		}
	}
	read("openshell")
	if err := runRuntime([]string{"default", "host"}, io.Discard); err == nil {
		t.Fatal("accepted host runtime")
	}
	read("openshell")
	if err := runRuntime([]string{"default", "typo"}, io.Discard); err == nil {
		t.Fatal("invalid preference accepted")
	}
	read("openshell")
	if err := runRuntime([]string{"default", "openshell"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	read("openshell")
	if err := os.WriteFile(filepath.Join(home, "runtime.json"), []byte(`{"default":"typo"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runRuntime([]string{"default"}, io.Discard); err == nil {
		t.Fatal("invalid stored default silently fell back")
	}
}

func TestRuntimeAnyUserPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PADDOCK_HOME", home)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"configure", "--binary", exe, "--gateway", "test", "--image", "sha256:fixture", "--policy", filepath.Join(home, "policy.json"), "--allow", "*"}
	if err := runRuntime(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := runRuntime([]string{"configure", "--allow", "*,alice"}, io.Discard); err == nil {
		t.Fatal("accepted an ambiguous wildcard list")
	}
	s, err := paddock.OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.OpenShellConfig()
	if err != nil || len(c.WatchAuthors) != 1 || c.WatchAuthors[0] != "*" {
		t.Fatalf("wildcard policy did not survive invalid update: %+v %v", c.WatchAuthors, err)
	}
}
