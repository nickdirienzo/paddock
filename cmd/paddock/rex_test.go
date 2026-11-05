package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRexLauncherFindsToolsWithMinimalServerEnvironment(t *testing.T) {
	dir := t.TempDir()
	tools := filepath.Join(dir, "mise tools with spaces")
	if err := os.MkdirAll(tools, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "codex"), []byte("#!/bin/sh\nprintf 'found-host-codex\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "paddock")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ncodex --version\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("PADDOCK_CODEX_BIN", filepath.Join(tools, "codex"))
	t.Setenv("GITHUB_TOKEN", "must-not-be-saved")
	command := rexLauncherCommand(executable)
	for _, arg := range command {
		if strings.Contains(arg, "GITHUB_TOKEN") || strings.Contains(arg, "must-not-be-saved") {
			t.Fatal("saved unrelated shell credentials")
		}
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "found-host-codex\n" {
		t.Fatalf("launcher lost installed tool paths: %s %v", output, err)
	}
}

func TestRexSetupPreservesConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "init.lua")
	prefix := "-- my shortcuts\nrex.bind(\"cmd+d\", \"pane.split\")\n"
	suffix := "-- more user config\n"
	if err := os.WriteFile(path, []byte(prefix), 0600); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	setup := func(exe, shortcut string) {
		t.Helper()
		if err := configureRexLauncher(path, exe, shortcut, false); err != nil {
			t.Fatal(err)
		}
	}
	setup("/path with spaces/paddock", rexDefaultKey)
	first := read()
	setup("/path with spaces/paddock", rexDefaultKey)
	if read() != first {
		t.Fatal("setup was not idempotent")
	}
	if err := os.WriteFile(path, []byte(first+suffix), 0600); err != nil {
		t.Fatal(err)
	}
	setup("/new/paddock", "ctrl+alt+p")
	updated := read()
	if !strings.HasPrefix(updated, prefix) || !strings.HasSuffix(updated, suffix) ||
		strings.Count(updated, rexConfigBegin) != 1 || strings.Contains(updated, "/path with spaces") ||
		!strings.Contains(updated, "[[/new/paddock]]") || !strings.Contains(updated, "[[ctrl+alt+p]]") {
		t.Fatal("setup lost user content or did not replace the launcher", updated)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("setup changed config permissions", info, err)
	}
	if err := configureRexLauncher(path, "", "", true); err != nil {
		t.Fatal(err)
	}
	if read() != prefix+suffix {
		t.Fatal("removal changed user configuration", read())
	}
}

func TestRexSetupRejectsMalformedMarkersAndSymlinks(t *testing.T) {
	for _, body := range []string{rexConfigBegin, rexConfigEnd, rexConfigEnd + rexConfigBegin, rexConfigBegin + rexConfigBegin + rexConfigEnd, "text" + rexConfigBegin + rexConfigEnd} {
		path := filepath.Join(t.TempDir(), "init.lua")
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if err := configureRexLauncher(path, "/bin/paddock", rexDefaultKey, false); err == nil {
			t.Fatal("accepted malformed config", body)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatal("changed malformed config", err)
		}
	}
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "init.lua")
	if err := os.WriteFile(target, []byte("user content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := configureRexLauncher(link, "/bin/paddock", rexDefaultKey, false); err == nil {
		t.Fatal("replaced symlink")
	}
}

func TestRexConfigLocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("REX_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	want := filepath.Join(os.Getenv("HOME"), ".config", "rex", "init.lua")
	if got, err := rexConfigPath(""); err != nil || got != want {
		t.Fatal(got, err)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, err := rexConfigPath(""); err != nil || got != filepath.Join(xdg, "rex", "init.lua") {
		t.Fatal(got, err)
	}
	custom := filepath.Join(t.TempDir(), "custom.lua")
	t.Setenv("REX_CONFIG", custom)
	if got, err := rexConfigPath(""); err != nil || got != custom {
		t.Fatal(got, err)
	}
	explicit := filepath.Join(t.TempDir(), "explicit.lua")
	if got, err := rexConfigPath(explicit); err != nil || got != explicit {
		t.Fatal(got, err)
	}
}
