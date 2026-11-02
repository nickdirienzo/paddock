package paddock

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestRepoAliases(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	s := openTestStore(t, home)
	source := repoFixture(t)
	bare := filepath.Join(t.TempDir(), "mirage.git")
	gitTest(t, source, "clone", "--bare", source, bare)
	canonical, err := s.AddRepo(ctx, "mirage", bare)
	if err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, home)
	if _, err = reopened.AddRepo(ctx, "mirage", bare); err != nil {
		t.Fatal("same mapping should be idempotent:", err)
	}
	if _, err = reopened.AddRepo(ctx, "mirage", source); err == nil {
		t.Fatal("silently remapped alias")
	}
	if _, err = reopened.AddRepo(ctx, "../bad", source); err == nil {
		t.Fatal("accepted invalid alias")
	}
	if _, err = reopened.AddRepo(ctx, "missing", t.TempDir()); err == nil {
		t.Fatal("accepted nonrepository")
	}
	got, err := ResolveRepoArgument(home, "mirage")
	if err != nil || got != canonical {
		t.Fatal(got, err)
	}
	for _, v := range []string{"./mirage", bare, "unregistered"} {
		got, err = ResolveRepoArgument(home, v)
		if err != nil || got != v {
			t.Fatal(got, err)
		}
	}
	if err = reopened.RemoveRepo("mirage"); err != nil {
		t.Fatal(err)
	}
	aliases, err := ReadRepoAliases(home)
	if err != nil || len(aliases) != 0 {
		t.Fatal(aliases, err)
	}
	if _, err = os.Stat(bare); err != nil {
		t.Fatal("removing alias affected repository:", err)
	}
	if err = reopened.RemoveRepo("mirage"); err == nil {
		t.Fatal("removed nonexistent alias")
	}
}

func TestRepoAliasCorruption(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	source := repoFixture(t)
	for _, content := range []string{`invalid JSON`, `{"version":2,"repositories":{}}`, `{"version":1,"repositories":{"mirage":"relative/path"}}`} {
		if err := os.WriteFile(filepath.Join(s.Home, "repos.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveRepoArgument(s.Home, "mirage"); err == nil {
			t.Fatal("ignored invalid config")
		}
		if _, err := s.AddRepo(context.Background(), "mirage", source); err == nil {
			t.Fatal("overwrote invalid config")
		}
		if path, err := ResolveRepoArgument(s.Home, source); err != nil || path != source {
			t.Fatal("explicit path depends on aliases", err)
		}
	}
}

func TestConcurrentRepoAliasWriters(t *testing.T) {
	home := t.TempDir()
	source := repoFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=^TestStoreHelper$")
			c.Env = append(os.Environ(), "PADDOCK_STORE_HELPER=1", "PADDOCK_STORE_TEST_HOME="+home, "PADDOCK_REPO_TEST_NAME="+fmt.Sprintf("repo-%d", i), "PADDOCK_REPO_TEST_PATH="+source)
			if b, err := c.CombinedOutput(); err != nil {
				t.Errorf("%v %s", err, b)
			}
		}(i)
	}
	wg.Wait()
	aliases, err := ReadRepoAliases(home)
	if err != nil || len(aliases) != 6 {
		t.Fatalf("lost alias updates: %v %v", aliases, err)
	}
}

func TestRepoTrustFollowsIdentityAndCanBeRevoked(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir())
	source := repoFixture(t)
	bare := filepath.Join(t.TempDir(), "mirage.git")
	gitTest(t, source, "clone", "--bare", source, bare)
	if _, err := s.AddRepo(ctx, "mirage", bare); err != nil {
		t.Fatal(err)
	}
	repo, err := ResolveRepository(ctx, bare, "")
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := RepoTrusted(s.Home, repo.CommonDir); err != nil || trusted {
		t.Fatal("trust must be opt-in", trusted, err)
	}
	if _, err = s.SetRepoTrust(ctx, "mirage", true); err != nil {
		t.Fatal(err)
	}
	// Alias changes must preserve the trust decision, including re-adding the same alias.
	if _, err = s.AddRepo(ctx, "mirage", bare); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddRepo(ctx, "other", source); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, s.Home)
	if trusted, err := RepoTrusted(reopened.Home, repo.CommonDir); err != nil || !trusted {
		t.Fatal("lost trust", trusted, err)
	}
	other, err := ResolveRepository(ctx, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := RepoTrusted(s.Home, other.CommonDir); err != nil || trusted {
		t.Fatal("trust leaked to another repository", trusted, err)
	}
	work := filepath.Join(t.TempDir(), "linked")
	gitTest(t, bare, "worktree", "add", "-b", "linked-test", work, "main")
	linked, err := ResolveRepository(ctx, work, "")
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := RepoTrusted(s.Home, linked.CommonDir); err != nil || !trusted {
		t.Fatal("linked worktree did not inherit trust", trusted, err)
	}
	if err = s.RemoveRepo("mirage"); err != nil {
		t.Fatal(err)
	}
	if trusted, err := RepoTrusted(s.Home, repo.CommonDir); err != nil || !trusted {
		t.Fatal("alias removal unexpectedly revoked identity trust", trusted, err)
	}
	if _, err = reopened.SetRepoTrust(ctx, work, false); err != nil {
		t.Fatal(err)
	}
	if trusted, err := RepoTrusted(s.Home, repo.CommonDir); err != nil || trusted {
		t.Fatal("trust not revoked", trusted, err)
	}
	if _, err = reopened.SetRepoTrust(ctx, t.TempDir(), true); err == nil {
		t.Fatal("trusted a non-repository")
	}
}
