package paddock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskArtifactsExposePatchAndUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if _, err := git(ctx, repo, "init"); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repo, "add", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := Task{ID: newID(), Worktree: repo, BaseCommit: base, Branch: "paddock/test", Runtime: "host"}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	artifacts, err := (Service{Store: store}).Artifacts(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if !artifacts.WorkspaceAvailable || !strings.Contains(artifacts.Patch, "+after") || len(artifacts.UntrackedFiles) != 1 || artifacts.UntrackedFiles[0] != "new.txt" {
		t.Fatalf("wrong artifacts: %+v", artifacts)
	}
}

func TestTaskPullRequestsUsePinnedRepositoryAndExactBranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/team/repo/pulls" || r.URL.Query().Get("head") != "team:paddock/abc" || r.URL.Query().Get("state") != "all" {
			t.Errorf("unexpected PR lookup: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected lookup", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[
{"number":7,"head":{"ref":"paddock/abc","repo":{"full_name":"team/repo"}},"base":{"repo":{"full_name":"team/repo"}}},
{"number":8,"head":{"ref":"other","repo":{"full_name":"team/repo"}},"base":{"repo":{"full_name":"team/repo"}}},
{"number":9,"head":{"ref":"paddock/abc","repo":{"full_name":"other/repo"}},"base":{"repo":{"full_name":"team/repo"}}},
{"number":10,"head":{"ref":"paddock/abc","repo":{"full_name":"team/repo"}},"base":{"repo":{"full_name":"other/repo"}}}
]`))
	}))
	defer server.Close()
	client := GitHubAPI{Identity: GitHubIdentity{Mode: "app"}, app: &githubAppAuth{config: GitHubAppConfig{InstallationID: 1}, token: "fixture", expires: time.Now().Add(time.Hour), client: server.Client(), baseURL: server.URL, clock: time.Now}}
	task := Task{Branch: "paddock/abc", OpenShell: &OpenShellTask{Repository: "team/repo"}}
	urls, err := taskBranchPullRequests(context.Background(), task, client)
	if err != nil || len(urls) != 1 || urls[0] != "https://github.com/team/repo/pull/7" {
		t.Fatal(urls, err)
	}
}
