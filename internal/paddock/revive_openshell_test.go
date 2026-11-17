package paddock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This gateway executes production bootstrap/context scripts only inside a
// disposable mapped filesystem. It never calls a real gateway or agent home.
const revivalFixtureGateway = `#!/usr/bin/env python3
import json,os,shutil,subprocess,sys
root=REVIVAL_ROOT
args=sys.argv[5:]
with open(root+'/calls','a') as f: f.write(json.dumps(args)+'\n')
def mapped(s):
 return s.replace('/home/sandbox',root+'/home').replace('/sandbox',root+'/sandbox')
if args[:2]==['sandbox','list']:
 states=[{'id':'expired-old-id','name':'expired-old-fixture'}] if os.path.exists(root+'/old-present') else []
 print(json.dumps({'sandboxes':states, 'next_page_token':''}))
elif args[:2]==['sandbox','create']:
 os.makedirs(root+'/sandbox',exist_ok=True);os.makedirs(root+'/home',exist_ok=True)
 print(json.dumps({'id':'fresh-fixture'}))
elif args[:2]==['settings','set']: pass
elif args[:2]==['sandbox','upload']:
 target=mapped(args[4]);os.makedirs(os.path.dirname(target),exist_ok=True);shutil.copyfile(args[3],target)
 if target.endswith('revival-context.md') and os.path.exists(root+'/corrupt-upload'):
  with open(target,'a') as f: f.write('tampered')
elif args[:2]==['sandbox','exec']:
 cmd=args[args.index('--')+1:]
 if cmd[0] in ['fixture-claude','fixture-codex'] and cmd[1:]==['--version']:
  print('fixture agent');sys.exit(0)
 cmd=[mapped(v) for v in cmd]
 env=os.environ.copy();env['HOME']=root+'/home'
 for i,v in enumerate(args):
  if v=='--env':
   k,value=args[i+1].split('=',1);env[k]=mapped(value)
 sys.exit(subprocess.call(cmd,env=env,cwd=mapped(args[args.index('--workdir')+1])))
else: sys.exit('unexpected fake gateway command: '+repr(args))
`

func openshellRevivalFixture(t *testing.T, agent string) (Service, Task, Repository, string, string) {
	t.Helper()
	ctx := context.Background()
	s, task, _ := gcFixture(t)
	// The old sandbox's task work is distinct from main. Revival must ignore it.
	gitTest(t, task.Repo, "push", "gc-origin", "HEAD:refs/heads/main")
	gitTest(t, task.Repo, "remote", "add", "origin", gitTest(t, task.Repo, "remote", "get-url", "gc-origin"))
	if err := os.WriteFile(filepath.Join(task.Repo, "new-main"), []byte("fresh upstream source"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, task.Repo, "add", "new-main")
	gitTest(t, task.Repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "advance main")
	gitTest(t, task.Repo, "push", "origin", "HEAD:refs/heads/main")
	// Source untracked files must stay only in the source checkout.
	if err := os.WriteFile(filepath.Join(task.Repo, "unrelated-untracked"), []byte("do not copy"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	encoded, _ := json.Marshal(root)
	gateway := filepath.Join(t.TempDir(), "openshell")
	if err := os.WriteFile(gateway, []byte(strings.ReplaceAll(revivalFixtureGateway, "REVIVAL_ROOT", string(encoded))), 0700); err != nil {
		t.Fatal(err)
	}
	task.Runtime = "openshell"
	task.Worktree = "/sandbox/worktree"
	task.Session.Agent = agent
	task.Session.AgentPath = "fixture-" + agent
	task.Session.ConversationID = newConversationID()
	task.Session.Generation = 3
	task.OpenShell = &OpenShellTask{Name: "expired-old-fixture", ID: "expired-old-id", Config: OpenShellConfig{Binary: gateway, Gateway: "fixture", Workspace: "fixture", Image: "sha256:fixture", Policy: "/fixture/policy", Providers: map[string]string{agent: "fixture-model"}}}
	transcript := []byte(`{"type":"user","message":{"role":"user","content":"archived shipped discussion"}}` + "\n")
	if agent == "codex" {
		transcript = []byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"archived shipped discussion"}]}}` + "\n")
	}
	a := ConversationArchive{Version: 2, TaskID: task.ID, Generation: 3, ConversationID: task.Session.ConversationID, Agent: agent, Runtime: "openshell", Worktree: task.Worktree, Transcript: transcript, SHA256: digest(transcript)}
	text, err := conversationContext(a)
	if err != nil {
		t.Fatal(err)
	}
	a.ContextText = text
	a.ContextSHA256 = digest([]byte(text))
	a.Transcript = nil
	a.SHA256 = a.ContextSHA256
	filename := task.ID + "-fixture.json"
	if err := os.MkdirAll(filepath.Join(s.Store.Home, "archives"), 0700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(s.Store.Home, "archives", filename)
	if err := writeJSONAtomic(archivePath, a); err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	task.Archive = &TaskArchiveState{Phase: "archived", File: filename, SHA256: digest(archiveBytes), SourceConversationID: task.Session.ConversationID, SourceGeneration: 3, PreviousWorktree: task.Worktree, PreviousBranch: task.Branch, ContextSHA256: a.ContextSHA256}
	if err := s.Store.update(task.ID, func(v *Task) error { *v = task; return nil }); err != nil {
		t.Fatal(err)
	}
	source, err := resolveFreshMain(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, source, text, root
}

func TestOpenShellRevivalPreparesFreshMainAndArchivedContext(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			s, task, source, text, root := openshellRevivalFixture(t, agent)
			archivePath := filepath.Join(s.Store.Home, "archives", task.Archive.File)
			before, err := os.ReadFile(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := s.prepareRevivedWorkspace(context.Background(), task, source, text)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := s.Store.Get(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.ID != task.ID || stored.ID != task.ID || stored.Archive.Phase != "revived" || stored.Status != "starting" || !stored.OpenShell.Ready || stored.OpenShell.ID != "fresh-fixture" || stored.OpenShell.Name == task.OpenShell.Name {
				t.Fatalf("wrong fresh task state: %+v", stored)
			}
			if stored.Session.Generation != 4 || stored.Session.Resume || stored.Session.RevivedFrom != task.Session.ConversationID {
				t.Fatalf("wrong new session: %+v", stored.Session)
			}
			if agent == "claude" && (stored.Session.ConversationID == "" || stored.Session.ConversationID == task.Session.ConversationID) {
				t.Fatal("Claude did not receive a new conversation ID")
			}
			if agent == "codex" && stored.Session.ConversationID != "" {
				t.Fatal("Codex should discover its new session ID at launch")
			}
			worktree := filepath.Join(root, "sandbox", "worktree")
			if got := gitTest(t, worktree, "rev-parse", "HEAD"); got != source.Commit || stored.BaseCommit != source.Commit {
				t.Fatal("fresh source main not seeded", got, source.Commit)
			}
			if _, err := os.Stat(filepath.Join(worktree, "new-main")); err != nil {
				t.Fatal("missing fresh main file", err)
			}
			if _, err := os.Stat(filepath.Join(worktree, "unrelated-untracked")); !os.IsNotExist(err) {
				t.Fatal("copied unrelated source file")
			}
			contextFile := filepath.Join(root, "home", ".paddock", "revival-context.md")
			bytes, err := os.ReadFile(contextFile)
			if err != nil || string(bytes) != text || digest(bytes) != stored.Session.ContextSHA256 {
				t.Fatal("context upload mismatch", err)
			}
			if err := s.verifyRevivalContext(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(contextFile)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("context not private", err)
			}
			if got, err := s.loadArchivedContext(stored); err != nil || got != text {
				t.Fatal("archive lost across new generation", err)
			}
			after, err := os.ReadFile(archivePath)
			if err != nil || string(before) != string(after) {
				t.Fatal("immutable archive changed", err)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Index(string(calls), `"list"`) > strings.Index(string(calls), `"create"`) || !strings.Contains(string(calls), `"list"`) {
				t.Fatal("created new sandbox before verifying old absent")
			}
			if strings.Contains(string(calls), `"delete"`) || strings.Contains(string(calls), "restore-native") {
				t.Fatal("unexpected destructive/native restore call")
			}
		})
	}
}

func TestOpenShellRevivalRejectsCorruptContextUpload(t *testing.T) {
	s, task, source, text, root := openshellRevivalFixture(t, "claude")
	if err := os.WriteFile(filepath.Join(root, "corrupt-upload"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareRevivedWorkspace(context.Background(), task, source, text); err == nil {
		t.Fatal("accepted corrupt sandbox context")
	}
	stored, err := s.Store.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Archive.Phase != "reviving" || stored.Status != "failed" {
		t.Fatalf("lost partial failure marker: %+v", stored)
	}
	if _, err := s.loadArchivedContext(stored); err != nil {
		t.Fatal("lost independent archive after failed revival", err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), `"delete"`) {
		t.Fatal("failed revival deleted a sandbox")
	}
}

func TestOpenShellRevivalRejectsOldSandboxStillPresent(t *testing.T) {
	s, task, source, text, root := openshellRevivalFixture(t, "claude")
	if err := os.WriteFile(filepath.Join(root, "old-present"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Store.path(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareRevivedWorkspace(context.Background(), task, source, text); err == nil {
		t.Fatal("accepted surviving old sandbox")
	}
	after, err := os.ReadFile(s.Store.path(task.ID))
	if err != nil || string(before) != string(after) {
		t.Fatal("changed task before absence confirmation", err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), `"create"`) || strings.Contains(string(calls), `"upload"`) {
		t.Fatal("provisioned before old sandbox was absent")
	}
	if _, err := s.loadArchivedContext(task); err != nil {
		t.Fatal("lost chat archive", err)
	}
}
