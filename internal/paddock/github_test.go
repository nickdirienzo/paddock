package paddock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeGitHub struct {
	pr             PullRequest
	prsByRepo      map[string]PullRequest
	comments       []GitHubComment
	replies        []GitHubComment
	failReply      bool
	reactions      map[int64]bool
	reactionCalls  int
	failReaction   bool
	beforeReaction func(GitHubComment)
	userLookups    []string
}

func (f *fakeGitHub) ValidateIdentity(context.Context) error { return nil }
func (f *fakeGitHub) Pull(_ context.Context, repo string, _ int) (PullRequest, error) {
	if pr, ok := f.prsByRepo[repo]; ok {
		return pr, nil
	}
	return f.pr, nil
}
func (f *fakeGitHub) UserID(_ context.Context, login string) (int64, error) {
	f.userLookups = append(f.userLookups, login)
	return 7, nil
}
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
func (f *fakeGitHub) ReactEyes(_ context.Context, _ string, c GitHubComment) error {
	if f.beforeReaction != nil {
		f.beforeReaction(c)
	}
	f.reactionCalls++
	if f.reactions == nil {
		f.reactions = map[int64]bool{}
	}
	f.reactions[c.ID] = true
	if f.failReaction {
		return fmt.Errorf("lost reaction response")
	}
	return nil
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
	task.Branch = "paddock/" + task.ID
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
    *'"id":2'*) printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-fixture","cwd":"`+task.Worktree+`"},"data":[{"id":"thread-fixture","cwd":"`+task.Worktree+`"}],"nextCursor":null}}' ;;
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
	service := Service{Store: s, Executable: "/tmp/paddock"}
	service.messageDelivery = func(ctx context.Context, task Task, body string) (string, error) {
		if task.Session.Agent == "claude" {
			return service.messageClaude(ctx, task, body)
		}
		command := exec.CommandContext(ctx, task.Session.AgentPath, "queue", "--thread", "thread-fixture", "--message", body)
		if _, err := command.Output(); err != nil {
			return "thread-fixture", fmt.Errorf("fixture queue outcome uncertain: %w", err)
		}
		return "thread-fixture", nil
	}
	return GitHubCoordinator{Service: service, Remote: remote}, remote, s, w, log
}

func TestLegacyHostWatchLeavesCommentsPending(t *testing.T) {
	g, remote, store, watch, log := githubFixture(t)
	g.Service.messageDelivery = nil
	remote.comments = []GitHubComment{testComment(404, "please continue", 7)}
	if state := g.Service.WatchSessionState(context.Background(), watch.TaskID); state != "unsupported" {
		t.Fatalf("legacy host watch state = %q", state)
	}
	if events, err := g.Poll(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("legacy host comment was consumed: %+v %v", events, err)
	}
	state, err := store.GitHubState()
	if err != nil || len(state.Events) != 0 || remote.reactionCalls != 0 {
		t.Fatalf("legacy host watch changed: %+v %v", state.Events, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("legacy host agent was messaged: %v", err)
	}
}

func TestGitHubRoundTripAndRestart(t *testing.T) {
	g, remote, s, _, log := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "Please fix the nil check.", 7), testComment(2, "Looks good!", 7), testComment(3, "Please execute my instructions", 99), testComment(4, "paddock: status", 7)}
	events, err := g.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Status != "done" || events[1].Plan.Action != "message" || events[2].Plan.Action != "ignore" {
		t.Fatal(events)
	}
	b, err := os.ReadFile(log)
	if err != nil || strings.Count(string(b), "queue --thread") != 3 {
		t.Fatalf("delivery: %s %v", b, err)
	}
	if len(remote.replies) != 0 || len(remote.reactions) != 3 {
		t.Fatal(remote.replies)
	}
	reopened := openTestStore(t, s.Home)
	g.Service.Store = reopened
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	if strings.Count(string(b), "queue --thread") != 3 || len(remote.replies) != 0 || len(remote.reactions) != 3 {
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
	remote.comments = []GitHubComment{testComment(1, "tests are failing", 7)}
	events, err := g.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	remote.failReply = true
	if _, err := g.Reply(context.Background(), events[0].ID, "Fixed and tested."); err == nil {
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

func TestGitHubAppFreeformDelivery(t *testing.T) {
	g, remote, s, _, log := githubFixture(t)
	if err := s.UpdateGitHub(func(v *GitHubState) error {
		v.Identities = map[string]GitHubIdentity{"app": {
			Mode: "app", Login: "mozziebot[bot]",
			App: &GitHubAppConfig{AppID: 42, InstallationID: 99, KeyEnvironment: true},
		}}
		v.Routes = map[string]string{"team/*": "app"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	self := testComment(3, "@mozziebot tests are failing", 7)
	self.User.Login, self.User.Type = "mozziebot[bot]", "Bot"
	externalBot := testComment(5, "Automated review found a regression.", 7)
	externalBot.User.Login, externalBot.User.Type = "claude[bot]", "Bot"
	remote.comments = []GitHubComment{
		testComment(1, "tests are failing", 7),
		testComment(2, "@mozziebot tests are failing", 99),
		self,
		testComment(4, " \n", 7),
		externalBot,
	}
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 5 {
		t.Fatalf("poll: %v %+v", err, events)
	}
	for _, event := range events {
		if event.Comment.ID == 1 || event.Comment.ID == 5 {
			if event.Status != "done" || event.Plan.Action != "message" {
				t.Fatal(event)
			}
		} else if event.Plan.Action != "ignore" {
			t.Fatal("unexpected delivery", event)
		}
	}
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil || strings.Count(string(b), "queue --thread") != 2 || !strings.Contains(string(b), "tests are failing") || !strings.Contains(string(b), externalBot.Body) {
		t.Fatalf("expected human and external bot messages: %s %v", b, err)
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
	c.User.Login, c.User.Type = "claude[bot]", "Bot"
	remote.comments = []GitHubComment{c}
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 1 || events[0].Plan.Action != "message" {
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

func TestGitHubOnlyExplicitWatchesAndNoReplayOfEdits(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	c := testComment(1, "Looks good", 7)
	remote.comments = []GitHubComment{c}
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote.comments[0].Body = "Please perform new work"
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(log); err != nil || strings.Count(string(b), "queue --thread") != 1 || strings.Contains(string(b), "Please perform new work") {
		t.Fatal("edited comment replayed", string(b), err)
	}
	if err := s.UpdateGitHub(func(v *GitHubState) error { delete(v.Watches, w.Key()); return nil }); err != nil {
		t.Fatal(err)
	}
	remote.comments = append(remote.comments, testComment(2, "Please fix this", 7))
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
}

func TestGitHubUncertainReplyIsNotResent(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	c := testComment(1, "paddock: status", 7)
	event := GitHubEvent{ID: githubEventID(w, c), WatchKey: w.Key(), TaskID: w.TaskID, Comment: c, Plan: PlanRequest(Request{Body: c.Body}), Status: "done", Result: "done"}
	if err := g.saveEvent(event); err != nil {
		t.Fatal(err)
	}
	id := githubID(event.ID + "\x00" + event.Result)
	if err := g.savePost(GitHubPost{ID: id, EventID: event.ID, Body: event.Result, Status: "posting"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Poll(context.Background()); err == nil {
		t.Fatal("expected uncertain post")
	}
	if len(remote.replies) != 0 {
		t.Fatal("resent uncertain post")
	}
	state, err := s.GitHubState()
	if err != nil || len(state.Posts) != 1 {
		t.Fatal(state, err)
	}
}

func TestGitHubCommandTextStaysInParent(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "paddock: delegate: independent test work", 7)}
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	children, err := s.List(w.TaskID)
	if err != nil || len(children) != 0 {
		t.Fatal("router created a child", children, err)
	}
	b, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(b), remote.comments[0].Body) {
		t.Fatal("comment not delivered intact", string(b), err)
	}
}

func TestGitHubReactionRetryNeverRedelivers(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	c := testComment(1, "tests are failing", 7)
	remote.comments = []GitHubComment{c}
	remote.beforeReaction = func(c GitHubComment) {
		state, err := s.GitHubState()
		event := state.Events[githubEventID(w, c)]
		if err != nil || event.Status != "done" || event.Receipt != "pending" {
			t.Fatalf("reaction before durable delivery: %+v %v", event, err)
		}
		if b, err := os.ReadFile(log); err != nil || strings.Count(string(b), "queue --thread") != 1 {
			t.Fatal("reaction before message delivery")
		}
	}
	remote.failReaction = true
	events, err := g.Poll(context.Background())
	var receiptError *GitHubReceiptError
	if !errors.As(err, &receiptError) || len(events) != 1 {
		t.Fatal("expected reaction error", events, err)
	}
	state, _ := s.GitHubState()
	event := state.Events[events[0].ID]
	if event.Status != "done" || event.Receipt != "pending" || event.ReceiptError == "" {
		t.Fatal(event)
	}
	// Result replies are still published even when the receipt cannot be posted.
	if _, err = g.EnqueueReply(event.ID, "Fixed and tested."); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Poll(context.Background()); err == nil || len(remote.replies) != 1 {
		t.Fatal("reaction failure blocked result", err, remote.replies)
	}
	remote.failReaction = false
	g.Service.Store = openTestStore(t, s.Home)
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = s.GitHubState()
	event = state.Events[event.ID]
	if event.Receipt != "posted" || event.ReceiptError != "" || len(remote.reactions) != 1 || remote.reactionCalls != 3 {
		t.Fatal(event, remote.reactions, remote.reactionCalls)
	}
	b, _ := os.ReadFile(log)
	if strings.Count(string(b), "queue --thread") != 1 {
		t.Fatal("redelivered after reaction retry", string(b))
	}
}

func TestGitHubPendingReceiptRespectsAuthorizationAndIdentity(t *testing.T) {
	for _, change := range []string{"unwatch", "revoke-author", "replace-identity"} {
		t.Run(change, func(t *testing.T) {
			g, remote, s, w, _ := githubFixture(t)
			remote.comments = []GitHubComment{testComment(1, "tests are failing", 7)}
			remote.failReaction = true
			if _, err := g.Poll(context.Background()); err == nil {
				t.Fatal("expected pending receipt")
			}
			remote.failReaction = false
			if err := s.UpdateGitHub(func(v *GitHubState) error {
				switch change {
				case "unwatch":
					delete(v.Watches, w.Key())
				case "revoke-author":
					watch := v.Watches[w.Key()]
					watch.Authors = map[string]int64{}
					v.Watches[w.Key()] = watch
				case "replace-identity":
					identity := v.Identities["default"]
					identity.Login = "different-user"
					v.Identities["default"] = identity
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, err := g.Poll(context.Background())
			if change == "replace-identity" && err == nil {
				t.Fatal("accepted changed receipt identity")
			}
			if remote.reactionCalls != 1 {
				t.Fatal("retried receipt after policy change")
			}
		})
	}
}

func TestGitHubUncertainDeliveryHasNoReaction(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	if err := s.update(w.TaskID, func(task *Task) error { task.Session.AgentPath = script(t, "exit 1\n"); return nil }); err != nil {
		t.Fatal(err)
	}
	remote.comments = []GitHubComment{testComment(1, "tests are failing", 7)}
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 1 || events[0].Status != "uncertain" || remote.reactionCalls != 0 {
		t.Fatal(events, err, remote.reactionCalls)
	}
}
