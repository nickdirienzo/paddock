package paddock

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var dockerContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// hostSandboxWorktree uses OrbStack's existing live container file share. The
// Docker labels identify the workload container belonging to this exact task;
// supervisor containers and sandboxes from other workspaces are excluded.
func hostSandboxWorktree(ctx context.Context, task Task) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return sandboxWorktreeInShare(ctx, task, filepath.Join(home, "OrbStack", "docker", "containers"))
}

func sandboxWorktreeInShare(ctx context.Context, task Task, share string) (string, error) {
	if task.OpenShell == nil || !conversationUUID.MatchString(task.OpenShell.ID) || task.OpenShell.Name == "" || task.OpenShell.Config.Workspace == "" {
		return "", fmt.Errorf("task has incomplete OpenShell sandbox identity")
	}
	if !path.IsAbs(task.Worktree) || path.Clean(task.Worktree) == "/" {
		return "", fmt.Errorf("task has no sandbox worktree")
	}
	if info, err := os.Stat(share); err != nil || !info.IsDir() {
		return "", fmt.Errorf("live sandbox files are unavailable at %s; host shell inspection currently requires OrbStack's container file share", share)
	}
	docker, err := executable("PADDOCK_DOCKER_BIN", "docker")
	if err != nil {
		return "", fmt.Errorf("find sandbox container: %w", err)
	}
	b, err := output(ctx, docker, "ps",
		"--filter", "label=openshell.ai/sandbox-id="+task.OpenShell.ID,
		"--filter", "label=openshell.ai/sandbox-name="+task.OpenShell.Name,
		"--filter", "label=openshell.ai/sandbox-workspace="+task.OpenShell.Config.Workspace,
		"--filter", "label=openshell.ai/isolation-role=sandbox",
		"--format", "{{.Names}}")
	if err != nil {
		return "", fmt.Errorf("find sandbox container: %w", err)
	}
	names := strings.Fields(string(b))
	if len(names) != 1 || !dockerContainerName.MatchString(names[0]) {
		return "", fmt.Errorf("expected one local workload container for sandbox %s; found %d", task.OpenShell.Name, len(names))
	}
	root, err := filepath.EvalSymlinks(filepath.Join(share, names[0]))
	if err != nil {
		return "", fmt.Errorf("sandbox is not visible in the OrbStack file share: %w", err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path.Clean(task.Worktree), "/"))))
	if err != nil {
		return "", fmt.Errorf("sandbox worktree is not visible in the OrbStack file share: %w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("sandbox worktree resolves outside its container file share")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("sandbox worktree is not a directory: %s", dir)
	}
	return dir, nil
}
