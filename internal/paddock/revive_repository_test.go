package paddock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func freshMainFixture(t *testing.T) (Task, string) {
	t.Helper()
	repo := repoFixture(t)
	gitTest(t, repo, "branch", "-M", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitTest(t, repo, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "origin", "main")
	r, err := discoverRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return Task{Repo: r.Root, CommonDir: r.CommonDir}, remote
}

func TestResolveFreshMainFetchesAdvancedRemotePreservingSource(t *testing.T) {
	task, remote := freshMainFixture(t)
	before := gitTest(t, task.Repo, "rev-parse", "HEAD")
	branch := gitTest(t, task.Repo, "symbolic-ref", "HEAD")
	untracked := filepath.Join(task.Repo, "unrelated-untracked")
	if err := os.WriteFile(untracked, []byte("leave this exact content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "publisher")
	gitTest(t, task.Repo, "clone", "--branch", "main", remote, clone)
	gitTest(t, clone, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "advance main")
	gitTest(t, clone, "push", "origin", "main")
	latest := gitTest(t, clone, "rev-parse", "HEAD")
	r, err := resolveFreshMain(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != latest || r.Commit == before || r.Base != "origin/main" || r.CommonDir != task.CommonDir || r.Root != task.Repo {
		t.Fatalf("unexpected fresh source: %+v", r)
	}
	if gitTest(t, task.Repo, "rev-parse", "HEAD") != before || gitTest(t, task.Repo, "symbolic-ref", "HEAD") != branch {
		t.Fatal("changed source checkout")
	}
	data, err := os.ReadFile(untracked)
	if err != nil || string(data) != "leave this exact content\n" {
		t.Fatal("untracked source changed", err)
	}
	if got := gitTest(t, task.Repo, "status", "--porcelain"); got != "?? unrelated-untracked" {
		t.Fatalf("source status changed: %s", got)
	}
}

func TestResolveFreshMainFailsClosedWithStaleTrackingRef(t *testing.T) {
	for _, failure := range []string{"missing-main", "transport", "authorization"} {
		t.Run(failure, func(t *testing.T) {
			task, remote := freshMainFixture(t)
			if _, err := resolveFreshMain(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "missing-main":
				gitTest(t, remote, "update-ref", "-d", "refs/heads/main")
			case "transport":
				gitTest(t, task.Repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "absent.git"))
			case "authorization":
				denied := script(t, "echo 'authorization denied' >&2\nexit 1\n")
				gitTest(t, task.Repo, "config", "remote.origin.uploadpack", denied)
			}
			if _, err := resolveFreshMain(context.Background(), task); err == nil {
				t.Fatal("accepted stale origin/main after fetch failure")
			}
			if gitTest(t, task.Repo, "rev-parse", "refs/remotes/origin/main") == "" {
				t.Fatal("fixture lacks stale tracking ref")
			}
		})
	}
}

func TestResolveFreshMainRequiresSourceIdentityAndOrigin(t *testing.T) {
	task, _ := freshMainFixture(t)
	task.CommonDir = filepath.Join(t.TempDir(), "missing")
	if _, err := resolveFreshMain(context.Background(), task); err == nil {
		t.Fatal("accepted mismatched source")
	}
	task.CommonDir = ""
	gitTest(t, task.Repo, "remote", "remove", "origin")
	if _, err := resolveFreshMain(context.Background(), task); err == nil {
		t.Fatal("accepted source without origin")
	}
}

func TestResolveFreshMainCancelledDoesNotUseCachedRef(t *testing.T) {
	task, _ := freshMainFixture(t)
	if _, err := resolveFreshMain(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveFreshMain(ctx, task); err == nil {
		t.Fatal("accepted cached main after cancellation")
	}
}
