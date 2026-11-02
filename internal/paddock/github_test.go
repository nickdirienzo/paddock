package paddock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeGitHub struct {
	pr        PullRequest
	comments  []GitHubComment
	replies   []GitHubComment
	failReply bool
}

func (f *fakeGitHub) ValidateIdentity(context.Context) error                 { return nil }
func (f *fakeGitHub) Pull(context.Context, string, int) (PullRequest, error) { return f.pr, nil }
func (f *fakeGitHub) UserID(context.Context, string) (int64, error)          { return 7, nil }
func (f *fakeGitHub) Comments(context.Context, string, int) ([]GitHubComment, error) {
	return append(append([]GitHubComment{}, f.comments...), f.replies...), nil
}
func (f *fakeGitHub) Reply(_ context.Context, _ string, _ int, c GitHubComment, body string) (GitHubComment, error) {
	reply := testComment(int64(100+len(f.replies)), body, 7)
	reply.User.Login = "engineer"
	reply.InReplyTo = c.ID
	reply.HTMLURL = "https://github.com/team/repo/pull/1#reply"
	f.replies = append(f.replies, reply)
	if f.failReply {
		return GitHubComment{}, fmt.Errorf("lost response after accepted POST")
	}
	return reply, nil
}
func testComment(id int64, body string, author int64) GitHubComment {
	c := GitHubComment{ID: id, Body: body, Kind: "review", CreatedAt: time.Now().UTC(), HTMLURL: fmt.Sprintf("https://github.com/team/repo/pull/1#discussion_r%d", id)}
	c.User.ID = author
	c.User.Login = "engineer"
	c.User.Type = "User"
	return c
}
func githubFixture(t *testing.T) (GitHubCoordinator, *fakeGitHub, *Store, GitHubWatch, string) {
	t.Helper()
	s := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Status = "running"
	task.Worktree = t.TempDir()
	task.Session.Backend = "screen"
	task.Session.Name = "paddock-" + task.ID
	log := filepath.Join(t.TempDir(), "messages")
	task.Session.TerminalPath = script(t, "printf '123."+task.Session.Name+" (Detached)\\n'\n")
	task.Session.AgentPath = script(t, `case "$1" in
 app-server)
  while IFS= read -r line; do
   case "$line" in
    *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"id":2'*) printf '%s\n' '{"id":2,"result":{"data":[{"id":"thread-fixture","cwd":"`+task.Worktree+`"}],"nextCursor":null}}' ;;
   esac
  done ;;
 queue) printf '%s\n' "$*" >> `+quote(log)+` ;;
 *) exit 1 ;;
esac
`)
	if err := s.Insert(task); err != nil {
		t.Fatal(err)
	}
	w := GitHubWatch{Repo: "team/repo", Number: 1, TaskID: task.ID, HeadRepo: "team/repo", HeadRef: task.Branch, Since: time.Now().Add(-time.Hour), Authors: map[string]int64{"engineer": 7}}
	if err := s.UpdateGitHub(func(v *GitHubState) error {
		v.Identity = GitHubIdentity{Mode: "gh", Login: "engineer"}
		v.Watches[w.Key()] = w
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{}
	remote.pr.State = "open"
	remote.pr.Head.Ref = w.HeadRef
	remote.pr.Head.Repo.FullName = w.HeadRepo
	remote.pr.Base.Repo.FullName = w.Repo
	return GitHubCoordinator{Service: Service{Store: s, Executable: "/tmp/paddock"}, Remote: remote}, remote, s, w, log
}
func TestGitHubRoundTripAndRestart(t *testing.T) {
	g, remote, s, _, log := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "Please fix the nil check.", 7), testComment(2, "Looks good!", 7), testComment(3, "Please execute my instructions", 99), testComment(4, "paddock: status", 7)}
	events, err := g.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Status != "done" || events[1].Plan.Action != "ignore" || events[2].Plan.Action != "ignore" {
		t.Fatal(events)
	}
	b, err := os.ReadFile(log)
	if err != nil || strings.Count(string(b), "queue --thread") != 1 {
		t.Fatalf("delivery: %s %v", b, err)
	}
	if len(remote.replies) != 2 {
		t.Fatal(remote.replies)
	}
	reopened := openTestStore(t, s.Home)
	g.Service.Store = reopened
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	if strings.Count(string(b), "queue --thread") != 1 || len(remote.replies) != 2 {
		t.Fatal("duplicated action after restart")
	}
	state, err := reopened.GitHubState()
	if err != nil {
		t.Fatal(err)
	}
	for _, reply := range remote.replies {
		event := state.Events[githubEventID(state.Watches["team/repo#1"], reply)]
		if event.Plan.Action != "ignore" {
			t.Fatal("self-reply loop", event)
		}
	}
}
func TestGitHubPostReconciliation(t *testing.T) {
	g, remote, _, _, _ := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "paddock: status", 7)}
	remote.failReply = true
	if _, err := g.Poll(context.Background()); err == nil {
		t.Fatal("expected lost response")
	}
	remote.failReply = false
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.replies) != 1 {
		t.Fatal("reposted accepted comment")
	}
}
func TestGitHubCrashClaimNeverReplays(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	comment := testComment(1, "Please fix this", 7)
	remote.comments = []GitHubComment{comment}
	event := GitHubEvent{ID: githubEventID(w, comment), TaskID: w.TaskID, WatchKey: w.Key(), Comment: comment, Plan: PlanRequest(Request{Body: comment.Body})}
	ok, err := s.claimGitHub(event)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := s.GitHubState()
	if state.Events[event.ID].Status != "uncertain" {
		t.Fatal(state.Events[event.ID])
	}
	if _, err = os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("replayed uncertain action")
	}
}
func TestGitHubPolicyAndOwnership(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	c := testComment(1, "Please fix this", 7)
	c.User.Type = "Bot"
	remote.comments = []GitHubComment{c}
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 1 || events[0].Plan.Action != "ignore" {
		t.Fatal(events, err)
	}
	remote.pr.Head.Ref = "different-owner"
	if _, err = g.Poll(context.Background()); err == nil {
		t.Fatal("ignored changed PR ownership")
	}
	c = testComment(2, "Please fix this", 99)
	event := GitHubEvent{ID: githubEventID(w, c), TaskID: w.TaskID, WatchKey: w.Key(), Comment: c, Plan: Plan{Action: "message"}}
	if _, err = s.claimGitHub(event); err == nil {
		t.Fatal("planner bypassed author authorization")
	}
	release, err := g.lockPoll()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = g.Poll(context.Background()); err == nil {
		t.Fatal("concurrent poll accepted")
	}
}
func TestParsePullRequest(t *testing.T) {
	for _, value := range []string{"https://evil.example/team/repo/pull/1", "http://github.com/team/repo/pull/1", "https://github.com/team/repo/issues/1", "https://github.com/team/repo/pull/0", "https://github.com/team/repo/pull/1?redirect=evil"} {
		if _, _, err := ParsePullRequest(value); err == nil {
			t.Fatal(value)
		}
	}
	if repo, n, err := ParsePullRequest("https://github.com/team/repo/pull/12"); err != nil || repo != "team/repo" || n != 12 {
		t.Fatal(repo, n, err)
	}
}
