package paddock

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillSetupUpdateAndRemove(t *testing.T) {
	home := t.TempDir()
	entries, err := SkillInstallations(home, "")
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	for _, entry := range entries {
		if entry.State != "missing" {
			t.Fatal(entry)
		}
	}
	entries, err = ConfigureSkills(home, "all", "/opt/paddock", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.State != "managed" {
			t.Fatal(entry)
		}
		body, err := os.ReadFile(entry.Path)
		if err != nil || string(body) != SkillStub("/opt/paddock") {
			t.Fatal("incorrect installed stub", err)
		}
	}
	if _, err = ConfigureSkills(home, "codex", "/new/paddock", false); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(entries[0].Path)
	if string(body) != SkillStub("/new/paddock") {
		t.Fatal("managed skill did not update")
	}
	body, _ = os.ReadFile(entries[1].Path)
	if string(body) != SkillStub("/opt/paddock") {
		t.Fatal("modified unselected agent")
	}
	extra := filepath.Join(filepath.Dir(entries[0].Path), "notes.txt")
	if err = os.WriteFile(extra, []byte("user notes"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		removed, err := ConfigureSkills(home, "all", "", true)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range removed {
			if entry.State != "missing" {
				t.Fatal(entry)
			}
		}
	}
	if body, err = os.ReadFile(extra); err != nil || string(body) != "user notes" {
		t.Fatal("removed user resource", err)
	}
	if _, err = SkillInstallations(home, "invalid"); err == nil {
		t.Fatal("accepted unknown agent")
	}
}

func TestSkillPreservesConflicts(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmanaged", true: "symlink"}[symlink], func(t *testing.T) {
			home := t.TempDir()
			entries, err := SkillInstallations(home, "all")
			if err != nil {
				t.Fatal(err)
			}
			path := entries[1].Path
			if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if symlink {
				target := filepath.Join(t.TempDir(), "skill")
				if err = os.WriteFile(target, []byte(SkillStub("/old/paddock")), 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err = os.WriteFile(path, []byte("user-owned skill"), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			for _, remove := range []bool{false, true} {
				if _, err = ConfigureSkills(home, "all", "/opt/paddock", remove); err == nil {
					t.Fatal("changed conflicting skill")
				}
				if _, err = os.Stat(entries[0].Path); !os.IsNotExist(err) {
					t.Fatal("partially installed despite conflict")
				}
				after, _ := os.ReadFile(path)
				if string(after) != string(before) {
					t.Fatal("conflicting skill modified")
				}
			}
		})
	}
}

func TestSkillReferenceInvokesExactExecutable(t *testing.T) {
	exe := script(t, "test \"$1\" = skill\ntest \"$2\" = show\nprintf 'guide-loaded'\n")
	stub := SkillStub(exe)
	_, rest, ok := strings.Cut(stub, "```sh\n")
	if !ok {
		t.Fatal("missing guide command")
	}
	command, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		t.Fatal("missing command terminator")
	}
	output, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil || string(output) != "guide-loaded" {
		t.Fatalf("guide command failed: %v %s", err, output)
	}
}
