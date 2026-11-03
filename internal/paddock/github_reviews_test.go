package paddock

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func reviewFixture() (GitHubComment, []GitHubComment) {
	summary := testComment(50, "Please reconsider the API boundary as a whole.", 7)
	summary.Kind, summary.ReviewID, summary.ReviewState = "review_submission", 50, "CHANGES_REQUESTED"
	summary.SubmittedAt = summary.CreatedAt
	first, second := testComment(1, "Use the shared abstraction.", 7), testComment(2, "Remove the duplicated adapter.", 7)
	first.ReviewID, second.ReviewID = 50, 50
	first.Path, second.Path = "api.go", "adapter.go"
	return summary, []GitHubComment{first, second}
}

func TestSubmittedReviewDeliveredTogetherWithIndividualReplies(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	summary, members := reviewFixture()
	// A draft comment can predate registration while its submission is new.
	members[0].CreatedAt = w.Since.Add(-time.Hour)
	remote.comments = append(members, summary)
	for range 2 {
		if _, err := g.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil || strings.Count(string(data), "queue --thread") != 1 {
		t.Fatalf("delivery: %s %v", data, err)
	}
	for _, text := range []string{summary.Body, members[0].Body, members[1].Body, "Read them together", "api.go", "adapter.go"} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("missing review context %q", text)
		}
	}
	state, _ := s.GitHubState()
	parent := state.Events[githubEventID(w, summary)]
	if parent.Status != "done" || len(parent.ReviewMembers) != 2 || parent.Receipt != "not_applicable" {
		t.Fatalf("parent: %+v", parent)
	}
	for _, c := range members {
		id := githubEventID(w, c)
		if !strings.Contains(string(data), "github reply "+id) || state.Events[id].Receipt != "posted" || state.Events[id].Status != "done" {
			t.Fatalf("missing child reply/receipt: %+v", state.Events[id])
		}
		if _, err := g.Reply(context.Background(), id, "Addressed together."); err != nil {
			t.Fatal(err)
		}
		if remote.replies[len(remote.replies)-1].InReplyTo != c.ID {
			t.Fatal("reply lost its inline thread")
		}
	}
	if remote.reactionCalls != 2 {
		t.Fatal("review summary must not receive a comment reaction", remote.reactionCalls)
	}
	// Later discussion and thread replies are independent messages.
	reply := testComment(3, "A follow-up on that adapter.", 7)
	reply.ReviewID, reply.InReplyTo = 50, 2
	discussion := testComment(4, "Another standalone instruction.", 7)
	discussion.Kind = "discussion"
	remote.comments = append(remote.comments, reply, discussion)
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(log)
	if strings.Count(string(data), "queue --thread") != 3 {
		t.Fatalf("standalone delivery: %s", data)
	}
}

func TestSubmittedBotReviewDeliveredTogether(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	summary, members := reviewFixture()
	for _, comment := range append([]*GitHubComment{&summary}, &members[0], &members[1]) {
		comment.User.Login, comment.User.Type = "claude[bot]", "Bot"
	}
	remote.comments = append(members, summary)
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil || strings.Count(string(data), "queue --thread") != 1 {
		t.Fatalf("delivery: %s %v", data, err)
	}
	for _, text := range []string{summary.Body, members[0].Body, members[1].Body} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("missing bot review context %q", text)
		}
	}
	state, _ := s.GitHubState()
	parent := state.Events[githubEventID(w, summary)]
	if parent.Plan.Action != "message" || len(parent.ReviewMembers) != 2 {
		t.Fatalf("bot review was not authorized: %+v", parent)
	}
}

func TestReviewWaitsForSubmissionAndDoesNotReplayOldComments(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	summary, members := reviewFixture()
	remote.comments = members
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("delivered partial review without its submission")
	}
	// Migration from the old poller: a previously delivered inline comment must
	// not run again when review grouping is introduced.
	if err := s.UpdateGitHub(func(v *GitHubState) error {
		id := githubEventID(w, members[0])
		v.Events[id] = GitHubEvent{ID: id, WatchKey: w.Key(), TaskID: w.TaskID, Comment: members[0], Status: "done", Receipt: "posted"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	remote.comments = append(remote.comments, summary)
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), members[0].Body) || !strings.Contains(string(data), members[1].Body) {
		t.Fatalf("migration replay: %s", data)
	}
}

func TestReviewDeliveryUncertainDoesNotReplayAnyMembers(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	summary, members := reviewFixture()
	remote.comments = append(members, summary)
	task, _ := s.Get(w.TaskID)
	source, _ := os.ReadFile(task.Session.AgentPath)
	// Simulate accepting the queue write, then losing its acknowledgement.
	source = []byte(strings.Replace(string(source), quote(log)+` ;;`, quote(log)+`; exit 1 ;;`, 1))
	if err := os.WriteFile(task.Session.AgentPath, source, 0700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _ = g.Poll(context.Background())
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), "queue --thread") != 1 || remote.reactionCalls != 0 {
		t.Fatalf("uncertain batch replayed: %s", data)
	}
	state, _ := s.GitHubState()
	for _, c := range append(members, summary) {
		if state.Events[githubEventID(w, c)].Status != "uncertain" {
			t.Fatalf("not uncertain: %+v", state.Events[githubEventID(w, c)])
		}
	}
}
