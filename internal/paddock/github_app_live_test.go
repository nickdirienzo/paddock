package paddock

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in acceptance against an existing local App profile and PR. This mints
// short-lived tokens and revokes only the token minted by this test. It posts
// nothing and never prints credentials, HTTP bodies, or child environments.
func TestLiveGitHubApp(t *testing.T) {
	raw := os.Getenv("PADDOCK_LIVE_GITHUB_APP_PR")
	if raw == "" {
		t.Skip("set PADDOCK_LIVE_GITHUB_APP_PR to opt into live App acceptance")
	}
	repo, number, err := ParsePullRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	home, err := StateHome()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.GitHubState()
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := state.ResolveGitHubIdentity(repo)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Mode != "app" || identity.App == nil {
		t.Fatal("live acceptance requires an App profile")
	}
	api := NewGitHubAPI(identity)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = api.ValidateIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = api.Pull(ctx, repo, number); err != nil {
		t.Fatal(err)
	}
	if _, err = api.Comments(ctx, repo, number); err != nil {
		t.Fatal(err)
	}
	t.Log("Authenticated App and read live PR plus conversation/review comments")
	first := api.app.token
	// Exercise the expiry path without waiting an hour. GitHub supplies the new
	// token; only the cached deadline is moved into the past.
	api.app.expires = time.Now().Add(-time.Minute)
	if _, err = api.Pull(ctx, repo, number); err != nil {
		t.Fatal(err)
	}
	if api.app.token == first {
		t.Fatal("expired cache did not obtain a new token")
	}
	t.Log("Expired cached deadline: native transport reminted and read live PR")
	revoked := api.app.token
	if _, err = api.app.http(ctx, "DELETE", "installation/token", revoked, nil, nil); err != nil {
		t.Fatal(err)
	}
	status, _ := api.app.http(ctx, "GET", "installation/repositories?per_page=1", revoked, nil, nil)
	if status != 401 {
		t.Fatalf("expected revoked token to return 401, got %d", status)
	}
	if _, err = api.Pull(ctx, repo, number); err != nil {
		t.Fatal(err)
	}
	if api.app.token == revoked {
		t.Fatal("401 did not refresh token")
	}
	t.Log("Real GitHub 401 after revocation: automatic refresh recovered")

	var key []byte
	if identity.App.KeyEnvironment {
		key = []byte(os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY"))
	} else {
		key, err = readProtectedKey(identity.App.KeyFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer clear(key)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", string(key))
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY_FILE", identity.App.KeyFile)
	// Use Paddock's production agent launcher with a probe executable at the
	// coding-agent boundary. Capture privately and report only pass/fail.
	task := taskFixture()
	task.Worktree = t.TempDir()
	task.Session.AgentPath = script(t, "env\n")
	cmd := AgentCommand(task, home, "/tmp/paddock")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	output, err := cmd.Output()
	if err != nil {
		t.Fatal("agent boundary probe failed")
	}
	defer clear(output)
	if bytes.Contains(output, key) || strings.Contains(string(output), "PADDOCK_GITHUB_PRIVATE_KEY") || strings.Contains(string(output), "PRIVATE KEY-----") {
		t.Fatal("App credential reached child")
	}
	t.Log("Actual App PEM supplied to parent was absent at the agent process boundary")
	journal, err := os.ReadFile(home + "/github.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{key, []byte(first), []byte(revoked), []byte(api.app.token)} {
		if len(secret) > 0 && bytes.Contains(journal, secret) {
			t.Fatal("credential persisted in GitHub state")
		}
	}
	t.Log("Local journal contains no private key or minted token")
}
