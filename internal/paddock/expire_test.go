package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func expiryFixture(t *testing.T) (Service, Task, gcChecks) {
	t.Helper()
	s, task, checks := gcFixture(t)
	task.ShippedAt = time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := s.Store.update(task.ID, func(v *Task) error { v.ShippedAt = task.ShippedAt; return nil }); err != nil {
		t.Fatal(err)
	}
	return s, task, checks
}
func TestExpireHostArchivesBeforeRemovingWorkspace(t *testing.T) {
	s, task, checks := expiryFixture(t)
	r, err := s.expireTask(context.Background(), task.ID, true, checks)
	if err != nil || !r.Removed {
		t.Fatalf("%+v %v", r, err)
	}
	stored, err := s.Store.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Archive == nil || stored.Archive.Phase != "archived" {
		t.Fatalf("%+v", stored.Archive)
	}
	if _, err = s.loadArchivedContext(stored); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(task.Worktree); !os.IsNotExist(err) {
		t.Fatal("workspace remains", err)
	}
	if _, err = os.Stat(task.Session.AgentHome); err != nil {
		t.Fatal("native home lost", err)
	}
	if gitTest(t, task.Repo, "rev-parse", "refs/heads/"+task.Branch) != task.ShippedCommit {
		t.Fatal("original branch lost")
	}
	cfg, err := s.Store.LifecycleConfig()
	if err != nil || cfg.WorktreeTTL != "168h" || len(cfg.DisposableDirectories) != 0 {
		t.Fatal("changed live default policy", cfg, err)
	}
}
func TestExpireHostRefusesUnsafeWork(t *testing.T) {
	for _, scenario := range []string{"warm", "unshipped", "active", "dirty", "unpushed", "process", "open-pr", "unknown-pr", "unknown-process", "archive-failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, task, checks := expiryFixture(t)
			switch scenario {
			case "warm":
				s.Store.update(task.ID, func(v *Task) error { v.ShippedAt = now(); return nil })
			case "unshipped":
				s.Store.update(task.ID, func(v *Task) error { v.ShippedAt = ""; return nil })
			case "active":
				s.Store.update(task.ID, func(v *Task) error { v.Status = "running"; return nil })
			case "dirty":
				if err := os.WriteFile(filepath.Join(task.Worktree, "untracked"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unpushed":
				gitTest(t, task.Worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "unpushed")
				head := gitTest(t, task.Worktree, "rev-parse", "HEAD")
				s.Store.update(task.ID, func(v *Task) error { v.ShippedCommit = head; return nil })
			case "process", "unknown-process":
				checks.processes = func(context.Context, Task) error { return fmt.Errorf("active or unknown process") }
			case "open-pr", "unknown-pr":
				checks.pulls = func(context.Context, Task) error { return fmt.Errorf("open or unknown PR") }
			case "archive-failure":
				if err := os.WriteFile(filepath.Join(s.Store.Home, "archives"), []byte("publication blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r, err := s.expireTask(context.Background(), task.ID, true, checks)
			if err != nil || r.Removed || r.Eligible {
				t.Fatalf("%+v %v", r, err)
			}
			if _, err = os.Stat(task.Worktree); err != nil {
				t.Fatal("unsafe workspace removed", err)
			}
		})
	}
}
func TestExpireHostDryRunMakesNoCheckpoint(t *testing.T) {
	s, task, checks := expiryFixture(t)
	r, err := s.expireTask(context.Background(), task.ID, false, checks)
	if err != nil || !r.Eligible || r.Removed {
		t.Fatalf("%+v %v", r, err)
	}
	stored, _ := s.Store.Get(task.ID)
	if stored.Archive != nil {
		t.Fatal("dry run checkpointed deletion")
	}
}

// Actual Git commands operate on a disposable fixture, while native paths and
// scratch checking are translated under the same temporary root.
const expiryGatewayScript = `#!/usr/bin/env python3
import glob,json,os,shutil,subprocess,sys
cfg=json.load(open(os.path.join(os.path.dirname(__file__),'config.json')))
root=cfg['root'];args=sys.argv[5:]
with open(root+'/calls','a') as f:f.write(json.dumps(args)+'\n')
def mapped(v):
 return v.replace('/home/sandbox',root+'/home').replace('/sandbox/worktree',cfg['worktree']).replace('/sandbox/repository.git',cfg['common']).replace('/sandbox',root+'/sandbox')
if args[:2]==['sandbox','get']:
 print(json.dumps({'id':'fixture-id','name':'fixture-name','phase':('Ready' if os.path.exists(root+'/running') else 'Stopped'),'labels':{'paddock.task':cfg['task']}}))
elif args[:2]==['sandbox','start']:open(root+'/running','w').write('running')
elif args[:2]==['sandbox','stop']:
 if os.path.exists(root+'/stop-failure'):sys.exit(1)
 if not os.path.exists(root+'/stop-noeffect'):
  try:os.unlink(root+'/running')
  except FileNotFoundError:pass
elif args[:2]==['sandbox','delete']:
 assert not os.path.exists(root+'/running'), 'cannot delete maintenance-running sandbox'
 task=json.load(open(cfg['record']))
 assert task['archive']['phase']=='deleting'
 archive=os.path.join(cfg['registry'],'archives',task['archive']['file'])
 assert os.path.getsize(archive)>0
 if os.path.exists(root+'/delete-failure'):sys.exit(1)
 open(root+'/deleted','w').write('deleted')
elif args[:2]==['sandbox','list']:
 p=root+'/list-response'
 print(open(p).read() if os.path.exists(p) else '{"sandboxes":[],"next_page_token":""}')
elif args[:2]==['sandbox','exec']:
 cmd=args[args.index('--')+1:]
 cmd=[mapped(v) for v in cmd]
 result=subprocess.run(cmd,stdout=subprocess.PIPE)
 out=result.stdout.decode()
 if cmd[0]=='git':out=out.replace(cfg['worktree'],'/sandbox/worktree').replace(cfg['common'],'/sandbox/repository.git')
 sys.stdout.write(out);sys.exit(result.returncode)
else:sys.exit('unexpected command '+repr(args))
`

func sandboxExpiryFixture(t *testing.T) (Service, Task, gcChecks, string) {
	t.Helper()
	s, task, checks := expiryFixture(t)
	root := t.TempDir()
	binDir := t.TempDir()
	for _, p := range []string{"sandbox", "home/.claude/projects/test"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	transcript := filepath.Join(root, "home/.claude/projects/test", task.Session.ConversationID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"sessionId":"`+task.Session.ConversationID+`","type":"assistant","message":{"role":"assistant","content":"shipped context"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{"root": root, "worktree": task.Worktree, "common": task.CommonDir, "task": task.ID, "record": filepath.Join(s.Store.Home, "tasks", task.ID+".json"), "registry": s.Store.Home}
	cfgBytes, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(binDir, "config.json"), cfgBytes, 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "openshell")
	if err := os.WriteFile(bin, []byte(expiryGatewayScript), 0700); err != nil {
		t.Fatal(err)
	}
	task.Runtime = "openshell"
	task.Worktree = "/sandbox/worktree"
	task.Session.AgentHome = "/home/sandbox/.claude"
	task.OpenShell = &OpenShellTask{ID: "fixture-id", Name: "fixture-name", Ready: true, Config: OpenShellConfig{Binary: bin, Gateway: "fixture", Workspace: "default"}}
	if err := s.Store.update(task.ID, func(v *Task) error { *v = task; return nil }); err != nil {
		t.Fatal(err)
	}
	return s, task, checks, root
}
func TestExpireSandboxCheckpointBeforeDelete(t *testing.T) {
	s, task, checks, root := sandboxExpiryFixture(t)
	r, err := s.expireTask(context.Background(), task.ID, true, checks)
	if err != nil || !r.Removed {
		t.Fatalf("%+v %v", r, err)
	}
	stored, _ := s.Store.Get(task.ID)
	if stored.Archive == nil || stored.Archive.Phase != "archived" {
		t.Fatalf("%+v", stored.Archive)
	}
	if _, err = s.loadArchivedContext(stored); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(root, "calls"))
	stopAt := strings.Index(string(calls), `["sandbox", "stop"`)
	deleteAt := strings.Index(string(calls), `["sandbox", "delete"`)
	if stopAt < 0 || deleteAt < stopAt || strings.Count(string(calls), `["sandbox", "stop"`) != 1 {
		t.Fatal("missing stop-before-delete ordering", string(calls))
	}
}
func TestExpireSandboxFailureRetainsCheckpointAndFailsClosed(t *testing.T) {
	for _, failure := range []string{"delete-failure", "list-unknown", "list-retains", "scratch", "archive-failure"} {
		t.Run(failure, func(t *testing.T) {
			s, task, checks, root := sandboxExpiryFixture(t)
			switch failure {
			case "delete-failure":
				os.WriteFile(filepath.Join(root, failure), nil, 0600)
			case "list-unknown":
				os.WriteFile(filepath.Join(root, "list-response"), []byte(`{"error":"unavailable"}`), 0600)
			case "list-retains":
				os.WriteFile(filepath.Join(root, "list-response"), []byte(`{"sandboxes":[{"id":"fixture-id","name":"fixture-name"}],"next_page_token":""}`), 0600)
			case "scratch":
				os.WriteFile(filepath.Join(root, "sandbox", "irreplaceable.txt"), []byte("preserve"), 0600)
			case "archive-failure":
				os.WriteFile(filepath.Join(s.Store.Home, "archives"), []byte("blocked"), 0600)
			}
			r, err := s.expireTask(context.Background(), task.ID, true, checks)
			if err != nil || r.Removed {
				t.Fatalf("%+v %v", r, err)
			}
			stored, _ := s.Store.Get(task.ID)
			if failure == "scratch" || failure == "archive-failure" {
				if stored.Archive != nil {
					t.Fatal("published deletion before guard/archive failure")
				}
				if _, err := os.Stat(filepath.Join(root, "deleted")); !os.IsNotExist(err) {
					t.Fatal("deleted despite failure")
				}
			} else {
				if stored.Archive == nil || stored.Archive.Phase != "deleting" {
					t.Fatalf("uncertain deletion lost tombstone: %+v", stored.Archive)
				}
				if _, err = s.loadArchivedContext(stored); err != nil {
					t.Fatal("lost checkpoint", err)
				}
			}
		})
	}
}
func TestConfirmSandboxAbsentRejectsIncompleteAndPaginatedUnknown(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"sandboxes":null,"next_page_token":""}`, `{"sandboxes":[]}`, `{"sandboxes":[],"next_page_token":"again"}`, `{"sandboxes":[{}],"next_page_token":""}`} {
		t.Run(body, func(t *testing.T) {
			_, task, _, root := sandboxExpiryFixture(t)
			if err := os.WriteFile(filepath.Join(root, "list-response"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := confirmSandboxAbsent(context.Background(), task); err == nil {
				t.Fatal("accepted incomplete absence evidence")
			}
		})
	}
}

func TestExpireRecoversDeletionTombstoneOnlyAfterVerifiedAbsence(t *testing.T) {
	for _, scenario := range []string{"present", "absent", "missing-context", "corrupt-context"} {
		t.Run(scenario, func(t *testing.T) {
			s, task, checks := expiryFixture(t)
			checkpoint, err := s.checkpointConversation(context.Background(), task)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint.Phase = "deleting"
			task.Archive = &checkpoint
			if err = s.Store.update(task.ID, func(v *Task) error { v.Archive = &checkpoint; return nil }); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "absent":
				gitTest(t, task.CommonDir, "worktree", "remove", "--", task.Worktree)
			case "missing-context":
				if err = os.Remove(filepath.Join(s.Store.Home, "archives", checkpoint.File)); err != nil {
					t.Fatal(err)
				}
			case "corrupt-context":
				if err = os.WriteFile(filepath.Join(s.Store.Home, "archives", checkpoint.File), []byte("interrupted"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r, err := s.expireTask(context.Background(), task.ID, true, checks)
			if err != nil {
				t.Fatal(err)
			}
			if r.Removed != (scenario == "absent") {
				t.Fatalf("%+v", r)
			}
			stored, _ := s.Store.Get(task.ID)
			expected := "deleting"
			if scenario == "absent" {
				expected = "archived"
			}
			if stored.Archive.Phase != expected {
				t.Fatalf("phase %s", stored.Archive.Phase)
			}
			if scenario != "absent" {
				if _, err = os.Stat(task.Worktree); err != nil {
					t.Fatal("retried uncertain deletion", err)
				}
			}
		})
	}
}

func TestExpireSandboxReconcilesUncertainDeleteWithoutRetry(t *testing.T) {
	s, task, checks, root := sandboxExpiryFixture(t)
	if err := os.WriteFile(filepath.Join(root, "list-response"), []byte(`{"error":"unavailable"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := s.expireTask(context.Background(), task.ID, true, checks); err != nil || r.Removed {
		t.Fatalf("%+v %v", r, err)
	}
	if err := os.Remove(filepath.Join(root, "list-response")); err != nil {
		t.Fatal(err)
	}
	r, err := s.expireTask(context.Background(), task.ID, true, checks)
	if err != nil || !r.Removed {
		t.Fatalf("%+v %v", r, err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), `["sandbox", "delete"`) != 1 {
		t.Fatal("retried destructive deletion", string(calls))
	}
}

func TestExpireSandboxRequiresConfirmedStopBeforeDelete(t *testing.T) {
	for _, failure := range []string{"stop-failure", "stop-noeffect"} {
		t.Run(failure, func(t *testing.T) {
			s, task, checks, root := sandboxExpiryFixture(t)
			if err := os.WriteFile(filepath.Join(root, failure), nil, 0600); err != nil {
				t.Fatal(err)
			}
			r, err := s.expireTask(context.Background(), task.ID, true, checks)
			if err != nil || r.Removed {
				t.Fatalf("%+v %v", r, err)
			}
			stored, _ := s.Store.Get(task.ID)
			if stored.Archive == nil || stored.Archive.Phase != "deleting" {
				t.Fatalf("lost uncertain-stop tombstone: %+v", stored.Archive)
			}
			if _, err = s.loadArchivedContext(stored); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), `["sandbox", "delete"`) || strings.Count(string(calls), `["sandbox", "stop"`) != 1 {
				t.Fatal("unsafe delete or duplicate stop", string(calls))
			}
			// A retry only queries authoritative absence, never retries stop or deletion.
			if err := os.WriteFile(filepath.Join(root, "list-response"), []byte(`{"sandboxes":[{"id":"fixture-id","name":"fixture-name"}],"next_page_token":""}`), 0600); err != nil {
				t.Fatal(err)
			}
			if r, err = s.expireTask(context.Background(), task.ID, true, checks); err != nil || r.Removed {
				t.Fatalf("%+v %v", r, err)
			}
			after, _ := os.ReadFile(filepath.Join(root, "calls"))
			if strings.Count(string(after), `["sandbox", "stop"`) != 1 || strings.Contains(string(after), `["sandbox", "delete"`) {
				t.Fatal("retried uncertain stop/delete")
			}
		})
	}
}

func TestExpireSandboxRejectsMalformedIdentityAndAgentHome(t *testing.T) {
	for _, scenario := range []string{"missing-sandbox", "missing-home", "wrong-home"} {
		t.Run(scenario, func(t *testing.T) {
			s, task, checks, root := sandboxExpiryFixture(t)
			if err := s.Store.update(task.ID, func(v *Task) error {
				switch scenario {
				case "missing-sandbox":
					v.OpenShell = nil
				case "missing-home":
					v.Session.AgentHome = ""
				case "wrong-home":
					v.Session.AgentHome = "/home/sandbox"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r, err := s.expireTask(context.Background(), task.ID, true, checks)
			if err != nil || r.Removed {
				t.Fatalf("%+v %v", r, err)
			}
			if _, err = os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
				t.Fatal("contacted sandbox for malformed source identity")
			}
		})
	}
}
