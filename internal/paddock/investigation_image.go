package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The host builds an investigation checkout as a root-owned image layer.
// OpenShell still runs the agent as its ordinary sandbox user. This is the
// ownership boundary that Landlock alone cannot supply for chmod and chown.
func buildInvestigationImage(ctx context.Context, baseImage, bundle string, t Task, dir string) (string, error) {
	baseTag := "paddock-investigation-base-" + t.ID + ":local"
	taskTag := "paddock-investigation-" + t.ID + ":local"
	if _, err := dockerForInvestigation(ctx, "tag", baseImage, baseTag); err != nil {
		return "", fmt.Errorf("pin the configured runtime image locally before investigation: %w", err)
	}
	defer func() { _, _ = dockerForInvestigation(context.Background(), "image", "rm", baseTag) }()
	buildDir := filepath.Join(dir, "image")
	if err := os.Mkdir(buildDir, 0700); err != nil {
		return "", err
	}
	if err := os.Link(bundle, filepath.Join(buildDir, "seed.bundle")); err != nil {
		return "", err
	}
	remote := ""
	if t.OpenShell.Provider != "" {
		remote = "https://github.com/" + t.OpenShell.Repository + ".git"
	}
	manifest, err := json.Marshal(struct {
		Branch string `json:"branch"`
		Remote string `json:"remote"`
		Name   string `json:"name"`
		Email  string `json:"email"`
	}{t.Branch, remote, t.OpenShell.GitAuthorName, t.OpenShell.GitAuthorEmail})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "prepare.json"), manifest, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "prepare.py"), []byte(investigationImagePreparation), 0600); err != nil {
		return "", err
	}
	dockerfile := "FROM " + baseTag + "\nUSER root\nCOPY seed.bundle /sandbox/seed.bundle\nCOPY prepare.json prepare.py /tmp/\nRUN python3 /tmp/prepare.py && rm /tmp/prepare.py /tmp/prepare.json\nUSER sandbox\nWORKDIR /home/sandbox\n"
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		return "", err
	}
	if _, err := dockerForInvestigation(ctx, "build", "--quiet", "--tag", taskTag, buildDir); err != nil {
		return "", err
	}
	output, err := dockerForInvestigation(ctx, "image", "inspect", taskTag, "--format", "{{.Id}}")
	if err != nil {
		return "", err
	}
	image := strings.TrimSpace(string(output))
	if !strings.HasPrefix(image, "sha256:") || len(image) != len("sha256:")+64 {
		return "", fmt.Errorf("Docker returned no pinned investigation image")
	}
	return image, nil
}

func dockerForInvestigation(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

const investigationImagePreparation = `import json
import os
import pwd
import subprocess

with open('/tmp/prepare.json') as source:
    task = json.load(source)
env = os.environ.copy()
env['GIT_CONFIG_NOSYSTEM'] = '1'
env['GIT_CONFIG_GLOBAL'] = '/dev/null'
def git(*args):
    subprocess.run(['git', '-c', 'core.hooksPath=/dev/null', *args], env=env, check=True)

git('clone', '--bare', '/sandbox/seed.bundle', '/sandbox/repository.git')
root = '/sandbox/repository.git'
if task['remote']:
    git('--git-dir=' + root, 'remote', 'set-url', 'origin', task['remote'])
    git('--git-dir=' + root, 'config', 'remote.origin.fetch', '+refs/heads/*:refs/remotes/origin/*')
else:
    git('--git-dir=' + root, 'remote', 'remove', 'origin')
git('--git-dir=' + root, 'config', 'user.name', task['name'])
git('--git-dir=' + root, 'config', 'user.email', task['email'])
git('--git-dir=' + root, 'worktree', 'add', '-b', task['branch'], '/sandbox/worktree', 'refs/heads/seed')
os.remove('/sandbox/seed.bundle')
user = pwd.getpwnam('sandbox')
for path in ('/sandbox/go-cache', '/sandbox/go-path', '/sandbox/.paddock'):
    os.makedirs(path, exist_ok=True)
    os.chown(path, user.pw_uid, user.pw_gid)
    os.chmod(path, 0o700)
os.chown('/sandbox', 0, 0)
os.chmod('/sandbox', 0o755)
`
