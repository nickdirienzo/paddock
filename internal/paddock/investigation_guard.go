package paddock

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

const investigationGuardDir = "/sandbox/.paddock/guard"

//go:embed investigation_exec.py
var investigationExec []byte

//go:embed investigation_launch.py
var investigationLaunch []byte

// installInvestigationGuard runs only after the trusted host has seeded the
// worktree. OpenShell remains the outer sandbox; each agent process adds a
// second, write-restricting Landlock layer before it starts its native daemon.
func installInvestigationGuard(ctx context.Context, t Task, hostDir string) error {
	if t.Provenance.Profile != "investigation" {
		return nil
	}
	if t.OpenShell == nil {
		return fmt.Errorf("investigation task has no OpenShell sandbox")
	}
	bootstrap := t
	bootstrap.Worktree = "/sandbox"
	if _, err := sandboxOutput(ctx, bootstrap, "mkdir", "-p", investigationGuardDir, "/sandbox/go-cache", "/sandbox/go-path"); err != nil {
		return err
	}
	for _, asset := range []struct {
		name string
		data []byte
	}{{"exec.py", investigationExec}, {"launch.py", investigationLaunch}} {
		local := filepath.Join(hostDir, asset.name)
		if err := os.WriteFile(local, asset.data, 0600); err != nil {
			return err
		}
		if _, err := t.OpenShell.Config.call(ctx, "sandbox", "upload", t.OpenShell.Name, local, investigationGuardDir+"/"+asset.name); err != nil {
			return err
		}
	}
	if _, err := sandboxOutput(ctx, bootstrap, "chmod", "500", investigationGuardDir+"/exec.py", investigationGuardDir+"/launch.py"); err != nil {
		return err
	}
	for _, agent := range []string{"codex", "claude"} {
		if _, err := sandboxOutput(ctx, bootstrap, "ln", "-s", "exec.py", investigationGuardDir+"/"+agent); err != nil {
			return err
		}
	}
	// Verify both sides of the boundary on every new sandbox. A broken or
	// unsupported Landlock setup fails task preparation before model execution.
	probe := `set -eu
test -r /sandbox/worktree/.git
printf allowed > /tmp/paddock-investigation-probe
rm /tmp/paddock-investigation-probe
if (printf forbidden > /sandbox/worktree/.paddock-investigation-probe) 2>/dev/null; then
  rm -f /sandbox/worktree/.paddock-investigation-probe
  exit 42
fi`
	if _, err := sandboxOutput(ctx, bootstrap, "/usr/bin/python3", investigationGuardDir+"/exec.py", "/bin/sh", "-c", probe); err != nil {
		return fmt.Errorf("investigation write guard failed its startup probe: %w", err)
	}
	return nil
}
