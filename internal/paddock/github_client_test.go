package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubCLIHelper(t *testing.T) {
	if os.Getenv("PADDOCK_GITHUB_HELPER") != "1" {
		return
	}
	args := os.Args
	path := ""
	method := ""
	for i, arg := range args {
		if strings.HasPrefix(arg, "repos/") || strings.HasPrefix(arg, "users/") || strings.HasPrefix(arg, "installation/") || arg == "user" {
			path = arg
		}
		if arg == "--method" && i+1 < len(args) {
			method = args[i+1]
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "fixture-token") {
			os.Exit(6)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	switch {
	case path == "user":
		enc.Encode(map[string]string{"login": "engineer"})
	case strings.HasPrefix(path, "installation/"):
		fmt.Print(`{"repositories":[]}`)
	case method == "POST":
		var body map[string]string
		if json.NewDecoder(os.Stdin).Decode(&body) != nil {
			os.Exit(7)
		}
		c := testComment(99, body["body"], 7)
		if path != "repos/team/repo/pulls/1/comments/42/replies" {
			os.Exit(8)
		}
		enc.Encode(c)
	case strings.Contains(path, "/issues/"):
		count := 100
		if strings.Contains(path, "page=2") {
			count = 1
		}
		comments := make([]GitHubComment, count)
		for i := range comments {
			comments[i] = testComment(int64(i+1), "discussion", 7)
		}
		enc.Encode(comments)
	case strings.Contains(path, "/pulls/"):
		enc.Encode([]GitHubComment{testComment(201, "review", 7)})
	default:
		os.Exit(9)
	}
	os.Exit(0)
}

func TestGitHubAppReactionEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-token" || json.NewDecoder(r.Body).Decode(&body) != nil || body["content"] != "eyes" {
			t.Error("invalid reaction request")
		}
		paths = append(paths, r.URL.Path)
		if len(paths) == 1 {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusOK) // Existing reactions are successful too.
		}
		fmt.Fprint(w, `{"id":123,"content":"eyes"}`)
	}))
	defer server.Close()
	api := NewGitHubAPI(GitHubIdentity{Mode: "app", App: &GitHubAppConfig{AppID: 42, InstallationID: 99}})
	api.app.baseURL = server.URL
	api.app.token = "fixture-token"
	api.app.expires = time.Now().Add(time.Hour)
	for _, kind := range []string{"discussion", "review", "review"} {
		// React on the exact reply comment, not its parent thread comment.
		if err := api.ReactEyes(context.Background(), "team/repo", GitHubComment{ID: 55, InReplyTo: 42, Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(paths, ",") != "/repos/team/repo/issues/comments/55/reactions,/repos/team/repo/pulls/comments/55/reactions,/repos/team/repo/pulls/comments/55/reactions" {
		t.Fatal(paths)
	}
	for _, c := range []GitHubComment{{ID: -1, Kind: "review"}, {ID: 55, Kind: "unknown"}} {
		if err := api.ReactEyes(context.Background(), "team/repo", c); err == nil {
			t.Fatal("accepted invalid reaction target")
		}
	}
}

func TestGCOpenPullLookupUsesExactHeadAndFailsClosed(t *testing.T) {
	status := 200
	body := `[]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/repos/team/repo/pulls" || r.URL.Query().Get("state") != "open" || r.URL.Query().Get("head") != "engineer:paddock/task" {
			t.Errorf("unsafe PR lookup: %s %s", r.Method, r.URL)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	api := NewGitHubAPI(GitHubIdentity{Mode: "app", App: &GitHubAppConfig{AppID: 42, InstallationID: 99}})
	api.app.baseURL, api.app.token, api.app.expires = server.URL, "fixture-token", time.Now().Add(time.Hour)
	if open, err := api.HasOpenPull(context.Background(), "team/repo", "engineer", "paddock/task"); err != nil || open {
		t.Fatal(open, err)
	}
	body = `[{"number":12,"state":"open"}]`
	if open, err := api.HasOpenPull(context.Background(), "team/repo", "engineer", "paddock/task"); err != nil || !open {
		t.Fatal(open, err)
	}
	status = 403
	if _, err := api.HasOpenPull(context.Background(), "team/repo", "engineer", "paddock/task"); err == nil {
		t.Fatal("unknown PR status treated as clear")
	}
}
func TestGitHubTransportPaginationIdentityAndThreading(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "gh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quote(os.Args[0])+" -test.run=^TestGitHubCLIHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PADDOCK_GITHUB_HELPER", "1")
	api := GitHubAPI{Identity: GitHubIdentity{Mode: "gh", Login: "engineer"}}
	if err := api.ValidateIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	comments, err := api.Comments(context.Background(), "team/repo", 1)
	if err != nil || len(comments) != 102 || comments[100].Kind != "discussion" || comments[101].Kind != "review" {
		t.Fatal(len(comments), err)
	}
	source := testComment(55, "question", 7)
	source.Kind = "review"
	source.InReplyTo = 42
	body := "literal ' $(touch SHOULD_NOT_EXIST)\nsecond line"
	reply, err := api.Reply(context.Background(), "team/repo", 1, source, body)
	if err != nil || reply.Body != body {
		t.Fatal(reply, err)
	}
}

func TestGitHubReviewSubmissionSnapshot(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/repos/team/repo/pulls/1/reviews":
			fmt.Fprint(w, `[{"id":50,"state":"CHANGES_REQUESTED","body":"Shared concern","submitted_at":"2026-11-03T19:00:00Z"},{"id":51,"state":"PENDING","body":"Unsubmitted","submitted_at":null}]`)
		case "/repos/team/repo/pulls/1/comments":
			fmt.Fprint(w, `[{"id":1,"pull_request_review_id":50,"body":"First"},{"id":2,"pull_request_review_id":50,"body":"Second"}]`)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer server.Close()
	api := NewGitHubAPI(GitHubIdentity{Mode: "app", App: &GitHubAppConfig{AppID: 42, InstallationID: 99}})
	api.app.baseURL, api.app.token, api.app.expires = server.URL, "fixture-token", time.Now().Add(time.Hour)
	comments, err := api.Comments(context.Background(), "team/repo", 1)
	if err != nil || len(comments) != 3 {
		t.Fatal(comments, err)
	}
	if paths[0] != "/repos/team/repo/pulls/1/reviews" || comments[0].Kind != "review_submission" || comments[0].CreatedAt.IsZero() || comments[1].ReviewID != 50 || comments[2].ReviewID != 50 {
		t.Fatal(paths, comments)
	}
}

func TestGitHubUserIDAllowsUsersAndBots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/engineer":
			fmt.Fprint(w, `{"id":7,"type":"User"}`)
		case "/users/claude[bot]":
			fmt.Fprint(w, `{"id":8,"type":"Bot"}`)
		case "/users/team":
			fmt.Fprint(w, `{"id":9,"type":"Organization"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := NewGitHubAPI(GitHubIdentity{Mode: "app", App: &GitHubAppConfig{AppID: 42, InstallationID: 99}})
	api.app.baseURL, api.app.token, api.app.expires = server.URL, "fixture-token", time.Now().Add(time.Hour)
	for login, expected := range map[string]int64{"engineer": 7, "claude[bot]": 8} {
		id, err := api.UserID(context.Background(), login)
		if err != nil || id != expected {
			t.Fatalf("%s: id=%d err=%v", login, id, err)
		}
	}
	if _, err := api.UserID(context.Background(), "team"); err == nil || !strings.Contains(err.Error(), "user or bot") {
		t.Fatalf("organization was authorized: %v", err)
	}
}
