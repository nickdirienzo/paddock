package paddock

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestGitHubAnyUserWatchAndRevocation(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	repo := repoFixture(t)
	if _, err := git(context.Background(), repo, "remote", "add", "origin", "https://github.com/team/repo.git"); err != nil {
		t.Fatal(err)
	}
	if err := s.update(w.TaskID, func(task *Task) error { task.Worktree = repo; return nil }); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/team/repo/pull/1"
	if err := g.Watch(context.Background(), url, w.TaskID, []string{" * "}); err != nil {
		t.Fatal(err)
	}
	state, _ := s.GitHubState()
	watch := state.Watches[w.Key()]
	if !watch.AllowAnyUser || len(watch.Authors) != 0 || len(remote.userLookups) != 0 || !watch.Since.Equal(w.Since) {
		t.Fatalf("wildcard registration: %+v, lookups=%v", watch, remote.userLookups)
	}
	unknown := testComment(1, "please fix this", 999)
	if !authorizedGitHub(watch, unknown) {
		t.Fatal("future user was denied")
	}
	// Narrowing the same watch preserves its cursor and revokes previously allowed
	// users at the action boundary, including queued replies and receipts.
	if err := g.Watch(context.Background(), url, w.TaskID, []string{"engineer"}); err != nil {
		t.Fatal(err)
	}
	state, _ = s.GitHubState()
	watch = state.Watches[w.Key()]
	if watch.AllowAnyUser || authorizedGitHub(watch, unknown) || !authorizedGitHub(watch, testComment(2, "hello", 7)) || !watch.Since.Equal(w.Since) {
		t.Fatalf("failed to narrow watch: %+v", watch)
	}
	_, err := s.claimGitHub(GitHubEvent{ID: "pending-wildcard-action", WatchKey: w.Key(), TaskID: w.TaskID, Comment: unknown, Plan: Plan{Action: "message"}})
	if err == nil {
		t.Fatal("revoked wildcard author could still act")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("registration executed work")
	}
}

func TestGitHubAnyUserPollFiltersAndDeduplicates(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	if err := s.UpdateGitHub(func(v *GitHubState) error {
		watch := v.Watches[w.Key()]
		watch.Authors = nil
		watch.AllowAnyUser = true
		v.Watches[w.Key()] = watch
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bot := testComment(2, "please fix", 88)
	bot.User.Type = "Bot"
	missingID := testComment(3, "please fix", 0)
	unknownType := testComment(4, "please fix", 91)
	unknownType.User.Type = ""
	remote.comments = []GitHubComment{testComment(1, "new user instruction", 99), bot, missingID, unknownType, testComment(5, "reply <!-- paddock:receipt -->", 92)}
	for range 2 {
		if _, err := g.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(log)
	if err != nil || strings.Count(string(b), "queue --thread") != 1 || !strings.Contains(string(b), "new user instruction") {
		t.Fatalf("wrong delivery: %s %v", b, err)
	}
	if remote.reactionCalls != 1 || !remote.reactions[1] {
		t.Fatal("incorrect eyes receipts", remote.reactions)
	}
}

func TestWatchAuthorValidation(t *testing.T) {
	for _, authors := range [][]string{{"*", "alice"}, {"*", "*"}, {""}, {"alice", " "}, {"bot[bot]"}, {"ali*"}} {
		if err := validateWatchAuthors(authors); err == nil {
			t.Fatalf("accepted invalid policy: %q", authors)
		}
	}
	for _, authors := range [][]string{nil, {"*"}, {" alice ", "bob"}} {
		if err := validateWatchAuthors(authors); err != nil {
			t.Fatalf("rejected valid policy: %q: %v", authors, err)
		}
	}
}

func TestGitHubAnyUserRejectsOldRunningPoller(t *testing.T) {
	g, _, s, w, _ := githubFixture(t)
	lock, ok, err := s.pollerLock("", false)
	if err != nil || !ok {
		t.Fatalf("poller fixture lock: %v", err)
	}
	defer releasePollerLock(lock)
	if err := writeJSONAtomic(s.pollerFile(".json"), GitHubPollerStatus{RuntimeVersion: 1}); err != nil {
		t.Fatal(err)
	}
	err = g.Watch(context.Background(), "https://github.com/team/repo/pull/1", w.TaskID, []string{"*"})
	if err == nil || !strings.Contains(err.Error(), "running poller does not support") {
		t.Fatalf("old poller accepted wildcard policy: %v", err)
	}
	state, _ := s.GitHubState()
	if state.Watches[w.Key()].AllowAnyUser || len(state.Watches[w.Key()].Authors) != 1 {
		t.Fatal("unsupported update changed the watch")
	}
}
