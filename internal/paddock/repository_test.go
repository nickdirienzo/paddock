package paddock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	v, e := git(context.Background(), dir, args...)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func repoFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo with spaces")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "initial")
	return dir
}
func TestRepositoryDefaultBranchAndIsolation(t *testing.T) {
	dir := repoFixture(t)
	ctx := context.Background()
	main := gitTest(t, dir, "rev-parse", "HEAD")
	gitTest(t, dir, "update-ref", "refs/remotes/origin/trunk", main)
	gitTest(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	gitTest(t, dir, "checkout", "-b", "feature")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "feature")
	if e := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("keep me"), 0600); e != nil {
		t.Fatal(e)
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0700)
	before := gitTest(t, dir, "status", "--porcelain=v1")
	head := gitTest(t, dir, "rev-parse", "HEAD")
	r, e := ResolveRepository(ctx, sub, "")
	if e != nil {
		t.Fatal(e)
	}
	if r.Base != "refs/remotes/origin/trunk" || r.Commit != main {
		t.Fatalf("%+v", r)
	}
	task := taskFixture()
	task.Repo = r.Root
	task.BaseCommit = r.Commit
	task.Branch = "paddock/" + task.ID
	task.Worktree = filepath.Join(t.TempDir(), "work tree")
	if e = CreateWorktree(ctx, task); e != nil {
		t.Fatal(e)
	}
	if gitTest(t, dir, "status", "--porcelain=v1") != before || gitTest(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("modified original checkout")
	}
	if gitTest(t, task.Worktree, "rev-parse", "HEAD") != main {
		t.Fatal("wrong base")
	}
	r2, e := ResolveRepository(ctx, task.Worktree, "")
	if e != nil || r2.CommonDir != r.CommonDir {
		t.Fatalf("linked worktree identity: %+v %v", r2, e)
	}
	override, e := ResolveRepository(ctx, dir, "feature")
	if e != nil || override.Commit != head {
		t.Fatalf("base override: %+v %v", override, e)
	}
}
func TestRepositoryErrorsAndInheritedGitEnv(t *testing.T) {
	if _, e := ResolveRepository(context.Background(), t.TempDir(), ""); e == nil {
		t.Fatal("accepted non-repo")
	}
	dir := repoFixture(t)
	t.Setenv("GIT_DIR", "/not/a/repository")
	t.Setenv("GIT_WORK_TREE", "/wrong")
	if _, e := ResolveRepository(context.Background(), dir, ""); e != nil {
		t.Fatal(e)
	}
	if _, e := ResolveRepository(context.Background(), dir, "missing"); e == nil {
		t.Fatal("accepted invalid base")
	}
	gitTest(t, dir, "branch", "-m", "unusual")
	if _, e := ResolveRepository(context.Background(), dir, ""); e == nil {
		t.Fatal("guessed current branch")
	}
	if _, e := ResolveRepository(context.Background(), dir, "unusual"); e != nil {
		t.Fatal(e)
	}
}

func TestBareRepositoryWorktrees(t *testing.T) {
	ctx := context.Background()
	source := repoFixture(t)
	before := gitTest(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "mirage.git")
	gitTest(t, source, "clone", "--bare", source, bare)
	// Match the remote-ref layout of the real Mirage repository.
	gitTest(t, bare, "update-ref", "refs/remotes/origin/main", before)
	gitTest(t, bare, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	head := gitTest(t, bare, "symbolic-ref", "HEAD")
	config, err := os.ReadFile(filepath.Join(bare, "config"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ResolveRepository(ctx, bare, "")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(bare)
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != canonical || r.CommonDir != canonical || r.Base != "refs/remotes/origin/main" || r.Commit != before {
		t.Fatalf("unexpected bare repository: %+v", r)
	}
	// Discovery also works from a subdirectory of the bare repository.
	sub, err := ResolveRepository(ctx, filepath.Join(bare, "objects"), "main")
	if err != nil || sub.Root != canonical || sub.Commit != before {
		t.Fatalf("%+v %v", sub, err)
	}
	first := taskFixture()
	first.Repo = r.Root
	first.BaseCommit = r.Commit
	first.Branch = "paddock/" + first.ID
	first.Worktree = filepath.Join(t.TempDir(), "task worktree")
	if err = CreateWorktree(ctx, first); err != nil {
		t.Fatal(err)
	}
	linked, err := ResolveRepository(ctx, first.Worktree, "")
	if err != nil || linked.CommonDir != canonical || linked.Commit != before {
		t.Fatalf("%+v %v", linked, err)
	}
	// Delegated work can reuse the bare repository without a primary checkout.
	second := taskFixture()
	second.Repo = r.Root
	second.BaseCommit = r.Commit
	second.Branch = "paddock/" + second.ID
	second.Worktree = filepath.Join(t.TempDir(), "child worktree")
	if err = CreateWorktree(ctx, second); err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{first, second} {
		if gitTest(t, task.Worktree, "rev-parse", "HEAD") != before {
			t.Fatal("wrong worktree base")
		}
	}
	if gitTest(t, bare, "symbolic-ref", "HEAD") != head || gitTest(t, bare, "rev-parse", "main") != before {
		t.Fatal("changed the source branch")
	}
	afterConfig, err := os.ReadFile(filepath.Join(bare, "config"))
	if err != nil || string(config) != string(afterConfig) {
		t.Fatal("changed repository configuration", err)
	}
	if gitTest(t, source, "status", "--porcelain=v1") != "" {
		t.Fatal("modified the original checkout")
	}
}

func TestPlainDirectoryNextToBareRepositoryIsNotGuessed(t *testing.T) {
	parent := t.TempDir()
	gitTest(t, parent, "init", "--bare", "mirage.git")
	plain := filepath.Join(parent, "mirage")
	if err := os.Mkdir(plain, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRepository(context.Background(), plain, ""); err == nil {
		t.Fatal("guessed a sibling repository")
	}
}
