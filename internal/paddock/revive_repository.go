package paddock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// resolveFreshMain refreshes only Git metadata in the host source repository.
// Revival must never fall back to a stale tracking ref or change/copy the source
// checkout, including its unrelated untracked files.
func resolveFreshMain(ctx context.Context, t Task) (Repository, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	source := t.Repo
	if source == "" {
		source = t.CommonDir
	}
	if source == "" {
		return Repository{}, fmt.Errorf("revival requires the host source repository")
	}
	r, err := discoverRepository(ctx, source)
	if err != nil {
		return Repository{}, fmt.Errorf("cannot discover revival source repository")
	}
	if t.CommonDir != "" {
		expected, e := filepath.EvalSymlinks(t.CommonDir)
		if e != nil || expected != r.CommonDir {
			return Repository{}, fmt.Errorf("revival source repository identity mismatch")
		}
	}
	// Explicit refspec ignores configured fetch mappings. Disable hooks, recursive
	// submodule fetching and maintenance; fetch never updates index or worktree.
	_, err = git(ctx, r.Root, "-c", "core.hooksPath="+os.DevNull, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "fetch.writeCommitGraph=false", "fetch", "--atomic", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--refmap=", "origin", "+refs/heads/main:refs/remotes/origin/main")
	if err != nil {
		// Transport errors can contain credential-bearing URLs; never return stderr.
		if ctx.Err() != nil {
			return Repository{}, fmt.Errorf("fetch current origin/main: %w", ctx.Err())
		}
		return Repository{}, fmt.Errorf("cannot fetch current origin/main; revival requires a successful fresh fetch")
	}
	r.Base = "origin/main"
	r.Commit, err = git(ctx, r.Root, "rev-parse", "--verify", "--end-of-options", "refs/remotes/origin/main^{commit}")
	if err != nil {
		return Repository{}, fmt.Errorf("fresh origin/main does not resolve to a commit")
	}
	if _, err = git(ctx, r.Root, "cat-file", "-e", r.Commit+"^{commit}"); err != nil {
		return Repository{}, fmt.Errorf("fresh origin/main commit is unavailable")
	}
	return r, nil
}
