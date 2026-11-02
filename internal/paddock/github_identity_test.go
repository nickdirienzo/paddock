package paddock

import (
	"context"
	"encoding/json"
	"testing"
)

func TestGitHubIdentityRoutePrecedence(t *testing.T) {
	s := GitHubState{Identities: map[string]GitHubIdentity{"personal": {Mode: "app", Login: "personal[bot]"}, "team": {Mode: "app", Login: "team[bot]"}, "dev": {Mode: "gh", Login: "engineer"}}, Routes: map[string]string{"nickdirienzo/*": "personal", "mirage-security/*": "team", "nickdirienzo/shared": "team"}}
	for _, tc := range []struct{ repo, name string }{{"nickdirienzo/paddock", "personal"}, {"mirage-security/mirage", "team"}, {"NickDiRienzo/Shared", "team"}} {
		b, _, err := s.ResolveGitHubIdentity(tc.repo)
		if err != nil || b.Name != tc.name {
			t.Fatal(b, err)
		}
	}
	if _, _, err := s.ResolveGitHubIdentity("unmapped/repo"); err == nil {
		t.Fatal("unmapped repository used implicit identity")
	}
	s.Routes["*"] = "dev"
	if b, _, err := s.ResolveGitHubIdentity("unmapped/repo"); err != nil || b.Name != "dev" {
		t.Fatal(b, err)
	}
	s.Routes["nickdirienzo/*"] = "missing"
	if _, _, err := s.ResolveGitHubIdentity("nickdirienzo/paddock"); err == nil {
		t.Fatal("missing profile fell back to another identity")
	}
}
func TestQueuedReplyPinsPrincipalAcrossRouteChanges(t *testing.T) {
	g, _, store, w, _ := githubFixture(t)
	personal := GitHubIdentity{Mode: "gh", Login: "engineer"}
	team := GitHubIdentity{Mode: "app", Login: "mirage-bot[bot]", App: &GitHubAppConfig{AppID: 42, InstallationID: 99, KeyEnvironment: true}}
	c := testComment(1, "Please fix this", 7)
	event := GitHubEvent{ID: githubEventID(w, c), WatchKey: w.Key(), TaskID: w.TaskID, Comment: c, Status: "done", Plan: Plan{Action: "message"}}
	if err := store.UpdateGitHub(func(v *GitHubState) error {
		v.Identities = map[string]GitHubIdentity{"personal": personal, "mirage": team}
		v.Routes = map[string]string{"team/*": "personal"}
		v.Events[event.ID] = event
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	personalRemote, teamRemote := &fakeGitHub{}, &fakeGitHub{}
	keyPersonal, _ := json.Marshal(personal)
	keyTeam, _ := json.Marshal(team)
	g.Remote = nil
	g.remotes = map[string]GitHubRemote{string(keyPersonal): personalRemote, string(keyTeam): teamRemote}
	post, err := g.EnqueueReply(event.ID, "Finished.")
	if err != nil || post.Identity.Name != "personal" {
		t.Fatal(post, err)
	}
	if err = store.RouteGitHubIdentity("team/*", "mirage"); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Reply(context.Background(), event.ID, "Finished."); err != nil {
		t.Fatal(err)
	}
	if len(personalRemote.replies) != 1 || len(teamRemote.replies) != 0 {
		t.Fatal("queued reply changed principal")
	}
	next, err := g.EnqueueReply(event.ID, "New update.")
	if err != nil || next.Identity.Name != "mirage" {
		t.Fatal(next, err)
	}
	// Replacing a profile with a different App cannot silently redirect its pending posts.
	if err = store.UpdateGitHub(func(v *GitHubState) error {
		i := v.Identities["mirage"]
		copy := *i.App
		copy.AppID = 43
		i.App = &copy
		v.Identities["mirage"] = i
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Reply(context.Background(), event.ID, "New update."); err == nil {
		t.Fatal("changed principal was accepted")
	}
	if len(teamRemote.replies) != 0 {
		t.Fatal("sent through replaced principal")
	}
}
func TestLegacyGitHubIdentityMigrationPinsOutbox(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	legacy := GitHubState{Version: 1, Identity: GitHubIdentity{Mode: "gh", Login: "engineer"}, Watches: map[string]GitHubWatch{}, Events: map[string]GitHubEvent{}, Posts: map[string]GitHubPost{"pending": {ID: "pending", Status: "posting"}}}
	if err := s.UpdateGitHub(func(v *GitHubState) error { *v = legacy; return nil }); err != nil {
		t.Fatal(err)
	}
	migrated, err := s.GitHubState()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := migrated.ResolveGitHubIdentity("team/repo")
	if err != nil || b.Name != "default" || b.Login != "engineer" {
		t.Fatal(b, err)
	}
	if migrated.Posts["pending"].Identity != b {
		t.Fatal("legacy uncertain post lost original principal")
	}
}

type identityValidationHook struct {
	GitHubRemote
	validate func() error
}

func (r identityValidationHook) ValidateIdentity(context.Context) error { return r.validate() }

func TestConcurrentQueueKeepsItsPrincipal(t *testing.T) {
	g, remote, store, watch, _ := githubFixture(t)
	comment := testComment(1, "Please fix this", 7)
	event := GitHubEvent{ID: githubEventID(watch, comment), WatchKey: watch.Key(), TaskID: watch.TaskID, Comment: comment, Status: "done"}
	if err := g.saveEvent(event); err != nil {
		t.Fatal(err)
	}
	g.Remote = identityValidationHook{GitHubRemote: remote, validate: func() error {
		if err := store.UpdateGitHub(func(v *GitHubState) error {
			v.Identities["team"] = GitHubIdentity{Mode: "gh", Login: "other-engineer"}
			v.Routes[watch.Repo] = "team"
			return nil
		}); err != nil {
			return err
		}
		_, err := g.EnqueueReply(event.ID, "Done.")
		return err
	}}
	if _, err := g.Reply(context.Background(), event.ID, "Done."); err == nil {
		t.Fatal("overwrote concurrently queued identity")
	}
	if len(remote.replies) != 0 {
		t.Fatal("sent with outdated identity")
	}
	state, err := store.GitHubState()
	if err != nil {
		t.Fatal(err)
	}
	post := state.Posts[githubID(event.ID+"\x00Done.")]
	if post.Identity.Name != "team" || post.Status != "queued" {
		t.Fatal("lost queued principal")
	}
}
