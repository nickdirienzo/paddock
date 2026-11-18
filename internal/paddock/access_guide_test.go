package paddock

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func guidePython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for sandbox guide fixture")
	}
	return python
}

func TestAccessGuideInstallerUpdatesReadableFileWithoutFollowingLinks(t *testing.T) {
	python := guidePython(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "control")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside")
	if err = os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	guide := filepath.Join(root, "access-guide.md")
	if err = os.Symlink(outside, guide); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(installSandboxAccessGuide, "/sandbox/.paddock", root)
	for range 2 {
		if out, err := exec.Command(python, "-c", script, sandboxAccessGuide).CombinedOutput(); err != nil {
			t.Fatalf("guide installation failed: %v: %s", err, out)
		}
	}
	raw, err := os.ReadFile(guide)
	if err != nil || string(raw) != sandboxAccessGuide {
		t.Fatalf("guide not readable/complete: %v", err)
	}
	info, err := os.Lstat(guide)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("guide mode: %v %v", info, err)
	}
	raw, err = os.ReadFile(outside)
	if err != nil || string(raw) != "keep" {
		t.Fatal("guide installer followed prior destination symlink")
	}
	redirected := filepath.Join(parent, "redirected")
	if err = os.Symlink(root, redirected); err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(installSandboxAccessGuide, "/sandbox/.paddock", redirected)
	if out, err := exec.Command(python, "-c", script, "changed").CombinedOutput(); err == nil {
		t.Fatalf("accepted redirected control directory: %s", out)
	}
	raw, _ = os.ReadFile(guide)
	if string(raw) != sandboxAccessGuide {
		t.Fatal("redirected installation changed original guide")
	}
}

func TestAccessGuideDiscoverableThroughExistingSandboxShortcut(t *testing.T) {
	python := guidePython(t)
	for _, args := range [][]string{{"--help"}, {"help"}, {}} {
		cmd := exec.Command(python, append([]string{"-c", sandboxShellClient}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sandbox guide discovery failed: %v: %s", err, out)
		}
		for _, want := range []string{"/sandbox/.paddock/access-guide.md", "paddock attach", "paddock message"} {
			if !strings.Contains(string(out), want) {
				t.Fatalf("missing discoverable %q: %s", want, out)
			}
		}
	}
}
