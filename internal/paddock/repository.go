package paddock

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repository struct{ Root, CommonDir, Base, Commit string }

// Ignore inherited Git location overrides: cwd is the repository contract.
func gitEnv() []string {
	var out []string
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		out = append(out, v)
	}
	return out
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	c.Env = gitEnv()
	var stderr bytes.Buffer
	c.Stderr = &stderr
	b, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(b)), nil
}

func discoverRepository(ctx context.Context, cwd string) (Repository, error) {
	var r Repository
	bare, err := git(ctx, cwd, "rev-parse", "--is-bare-repository")
	if err != nil {
		return r, fmt.Errorf("run Paddock inside a Git working tree or point --repo at a bare repository: %w", err)
	}
	rootFlag := "--show-toplevel"
	if bare == "true" {
		// Bare repositories have no working-tree root, but can create worktrees.
		rootFlag = "--absolute-git-dir"
	}
	root, err := git(ctx, cwd, "rev-parse", rootFlag)
	if err != nil {
		return r, err
	}
	r.Root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return r, err
	}
	r.CommonDir, err = git(ctx, r.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return r, err
	}
	r.CommonDir, err = filepath.EvalSymlinks(r.CommonDir)
	if err != nil {
		return r, err
	}
	return r, nil
}

func ResolveRepository(ctx context.Context, cwd, base string) (Repository, error) {
	r, err := discoverRepository(ctx, cwd)
	if err != nil {
		return r, err
	}
	if base != "" {
		r.Base = base
		r.Commit, err = git(ctx, r.Root, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
		return r, err
	}
	// Use locally known remote HEAD; never fetch or change the caller's checkout.
	var candidates []string
	if head, e := git(ctx, r.Root, "symbolic-ref", "refs/remotes/origin/HEAD"); e == nil {
		candidates = append(candidates, head)
	}
	candidates = append(candidates, "refs/remotes/origin/main", "refs/remotes/origin/master")
	if name, e := git(ctx, r.Root, "config", "--get", "init.defaultBranch"); e == nil && name != "" {
		candidates = append(candidates, "refs/heads/"+name)
	}
	candidates = append(candidates, "refs/heads/main", "refs/heads/master")
	for _, ref := range candidates {
		if commit, e := git(ctx, r.Root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}"); e == nil {
			r.Base = ref
			r.Commit = commit
			return r, nil
		}
	}
	return r, fmt.Errorf("cannot determine a default branch; fetch origin or specify --base <ref> (the repository must have a commit)")
}

func CreateWorktree(ctx context.Context, t Task) error {
	if err := os.MkdirAll(filepath.Dir(t.Worktree), 0700); err != nil {
		return err
	}
	_, err := git(ctx, t.Repo, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "-b", t.Branch, "--", t.Worktree, t.BaseCommit)
	return err
}
