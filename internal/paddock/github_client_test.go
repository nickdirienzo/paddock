package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
