package paddock

import (
	"context"
	"strings"
	"testing"
)

func TestIdentityOwnerProfilesAndImmutableProvenance(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateIdentity("mozzie", "100:engineer", []string{"investigation"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIdentity("mozzie", "100:engineer", nil); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	state, err := store.GitHubState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.IdentityForCaller("mozzie", "implementation", "100:engineer"); err == nil {
		t.Fatal("profile escalation accepted")
	}
	if _, err := state.IdentityForCaller("mozzie", "investigation", "101:other"); err == nil {
		t.Fatal("other caller accepted")
	}
	if _, err := state.IdentityForCaller("mozzie", "investigation", "100:engineer"); err != nil {
		t.Fatal(err)
	}
	task := Task{ID: newID(), Status: "preparing", Provenance: TaskProvenance{Caller: "100:engineer", Identity: "mozzie", Profile: "investigation", Context: map[string]string{"case": "123"}}}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	if err := store.update(task.ID, func(v *Task) error { v.Provenance.Context["case"] = "456"; return nil }); err == nil {
		t.Fatal("provenance mutation accepted")
	}
	got, err := store.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance.Context["case"] != "123" {
		t.Fatalf("persisted provenance changed: %+v", got.Provenance)
	}
}

func TestGitHubRouteRequiresIdentityOwner(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIdentity("mine", caller, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIdentity("other", "999999:other", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.RouteGitHubIdentity("team/project", "other"); err == nil {
		t.Fatal("caller routed a repository to another owner's identity")
	}
	if err := store.RouteGitHubIdentity("team/project", "mine"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateGitHub(func(v *GitHubState) error {
		v.Routes["team/owned-by-other"] = "other"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RouteGitHubIdentity("team/owned-by-other", "mine"); err == nil {
		t.Fatal("caller replaced another owner's route")
	}
}

func TestIntegrationDispatchIsolationAndExternalReconciliation(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIdentity("service", caller, []string{"investigation"}); err != nil {
		t.Fatal(err)
	}
	s := Service{Store: store}
	if _, err := s.Run(context.Background(), RunOptions{Runtime: "host", Identity: "service", Profile: "investigation", Prompt: "review"}); err == nil || !strings.Contains(err.Error(), "OpenShell") {
		t.Fatalf("host identity execution was accepted: %v", err)
	}
	task := Task{ID: newID(), Runtime: "openshell", Prompt: "review", Session: Session{Agent: "codex"}, Provenance: TaskProvenance{Caller: caller, Requester: "user-1", RequesterSource: "service-api", ExternalID: "event-1", Profile: "implementation", Context: map[string]string{"case": "123"}}}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindExternal(caller, "service-api", "event-1")
	if err != nil || !found || got.ID != task.ID {
		t.Fatalf("external lookup: %+v %t %v", got, found, err)
	}
	other := task
	other.ID = newID()
	if err := store.Insert(other); err == nil {
		t.Fatal("duplicate external ID accepted")
	}
	if _, found, _ := store.FindExternal("different-caller", "service-api", "event-1"); found {
		t.Fatal("other caller resolved task")
	}
	matched, err := s.Run(context.Background(), RunOptions{Prompt: "review", Requester: "user-1", RequesterSource: "service-api", ExternalID: "event-1", Context: map[string]string{"case": "123"}})
	if err != nil || matched.ID != task.ID {
		t.Fatalf("idempotent dispatch: %+v %v", matched, err)
	}
	if _, err := s.Run(context.Background(), RunOptions{Prompt: "review", Requester: "user-1", RequesterSource: "service-api", ExternalID: "event-1", Context: map[string]string{"case": "changed"}}); err == nil {
		t.Fatal("changed dispatch data accepted")
	}
	legacy := task
	legacy.ID = newID()
	legacy.Runtime = "host"
	legacy.Provenance.ExternalID = "legacy-host"
	if err := store.Insert(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), RunOptions{Prompt: "review", Requester: "user-1", RequesterSource: "service-api", ExternalID: "legacy-host", Context: map[string]string{"case": "123"}}); err == nil || !strings.Contains(err.Error(), "OpenShell") {
		t.Fatalf("reused a legacy host execution: %v", err)
	}
}

func TestExternalDispatchRejectsChangedRepositoryAndExecution(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	first, second := repoFixture(t), repoFixture(t)
	resolved, err := ResolveRepository(context.Background(), first, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: newID(), Repo: resolved.Root, Base: resolved.Base, BaseCommit: resolved.Commit, Runtime: "openshell", Prompt: "review", Session: Session{Agent: "codex", Backend: "screen"}, Provenance: TaskProvenance{Caller: caller, ExternalID: "job-1", Profile: "implementation"}}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	s := Service{Store: store}
	request := RunOptions{Prompt: "review", Repo: first, Base: "refs/heads/main", Runtime: "openshell", Agent: "codex", Backend: "screen", ExternalID: "job-1"}
	if got, err := s.Run(context.Background(), request); err != nil || got.ID != task.ID {
		t.Fatalf("matching retry failed: %+v %v", got, err)
	}
	for _, changed := range []RunOptions{
		func() RunOptions { v := request; v.Repo = second; return v }(),
		func() RunOptions { v := request; v.Base = "main"; return v }(),
		func() RunOptions { v := request; v.Runtime = "host"; return v }(),
		func() RunOptions { v := request; v.Backend = "rex"; return v }(),
	} {
		if _, err := s.Run(context.Background(), changed); err == nil {
			t.Fatalf("changed dispatch reused task: %+v", changed)
		}
	}
}

func TestDelegationRejectsAnotherCallersParent(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent := Task{ID: newID(), Status: "running", Provenance: TaskProvenance{Caller: "999999:other", Profile: "implementation"}}
	if err := store.Insert(parent); err != nil {
		t.Fatal(err)
	}
	s := Service{Store: store}
	if _, err := s.Run(context.Background(), RunOptions{Parent: parent.ID, Prompt: "follow up", Runtime: "openshell"}); err == nil || !strings.Contains(err.Error(), "owner of parent") {
		t.Fatalf("accepted another caller's parent: %v", err)
	}
}

func TestInvestigationGitHubProfileIsReadOnly(t *testing.T) {
	for _, capability := range profileCapabilities("investigation") {
		if capability == "github.write" || capability == "repository.write" {
			t.Fatal("investigation advertises write capability")
		}
	}
	profile := githubProfileForExecution("owner/repo", "task", "investigation")
	endpoints := profile["endpoints"].([]any)
	api := endpoints[0].(map[string]any)
	for _, item := range api["rules"].([]any) {
		allow := item.(map[string]any)["allow"].(map[string]string)
		if allow["method"] != "GET" {
			t.Fatalf("investigation permits %s", allow["method"])
		}
	}
	graphql := endpoints[1].(map[string]any)
	if graphql["access"] != "read-only" {
		t.Fatal("GraphQL write access retained")
	}
	git := endpoints[2].(map[string]any)
	for _, item := range git["rules"].([]any) {
		allow := item.(map[string]any)["allow"].(map[string]string)
		if strings.Contains(allow["path"], "git-receive-pack") {
			t.Fatal("git push retained")
		}
	}
}
