package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMergedShipmentMultiplePRs(t *testing.T) {
	stamp := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
	task := Task{Branch: "paddock/task", LastWorkAt: time.Now().Add(-96 * time.Hour).UTC().Format(time.RFC3339Nano)}
	links := []TaskPullRequest{{"owner/repo", 1, "owner/repo", "earlier"}, {"owner/repo", 2, "owner/repo", task.Branch}}
	makePR := func(n int) PullRequest {
		p := PullRequest{Number: n, State: "closed", Merged: true, MergedAt: stamp}
		p.Base.Ref = "main"
		p.Base.Repo.FullName = "owner/repo"
		p.Head.Ref = links[n-1].HeadRef
		p.Head.Repo.FullName = "owner/repo"
		p.Head.SHA = strings.Repeat("a", 40)
		return p
	}
	for _, name := range []string{"all merged", "open earlier", "closed unmerged", "unknown", "wrong base", "wrong head", "missing timestamp", "future timestamp", "no current branch", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			ls := append([]TaskPullRequest(nil), links...)
			if name == "no current branch" {
				ls = ls[:1]
			}
			if name == "duplicate" {
				ls = append(ls, ls[0])
			}
			calls := 0
			result, err := mergedShipment(context.Background(), task, "owner/repo", ls, func(_ context.Context, _ string, n int) (PullRequest, error) {
				calls++
				p := makePR(n)
				if n == 1 {
					switch name {
					case "open earlier":
						p.State = "open"
						p.Merged = false
					case "closed unmerged":
						p.Merged = false
					case "unknown":
						return p, errors.New("offline")
					case "wrong base":
						p.Base.Ref = "feature"
					case "wrong head":
						p.Head.Ref = "other"
					case "missing timestamp":
						p.MergedAt = ""
					case "future timestamp":
						p.MergedAt = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
					}
				}
				return p, nil
			})
			good := name == "all merged" || name == "duplicate"
			if good {
				if err != nil {
					t.Fatal(err)
				}
				if result.ShipmentSource != "github" || result.ShippedCommit != strings.Repeat("a", 40) || calls != 2 {
					t.Fatalf("bad evidence: %+v calls=%d", result, calls)
				}
			} else if err == nil {
				t.Fatal("unsafe shipment accepted")
			}
		})
	}
}
func TestMergedShipmentUsesLatestCurrentBranchHead(t *testing.T) {
	task := Task{Branch: "task"}
	links := []TaskPullRequest{{"o/r", 1, "o/r", "task"}, {"o/r", 2, "o/r", "task"}}
	for _, reverse := range []bool{false, true} {
		if reverse {
			links[0], links[1] = links[1], links[0]
		}
		result, err := mergedShipment(context.Background(), task, "o/r", links, func(_ context.Context, _ string, n int) (PullRequest, error) {
			p := PullRequest{Number: n, State: "closed", Merged: true, MergedAt: time.Now().Add(time.Duration(-100+n) * time.Hour).UTC().Format(time.RFC3339Nano)}
			p.Base.Ref = "main"
			p.Base.Repo.FullName = "o/r"
			p.Head.Ref = "task"
			p.Head.Repo.FullName = "o/r"
			p.Head.SHA = strings.Repeat(string(rune('a'+n)), 40)
			return p, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.ShippedCommit != strings.Repeat("c", 40) {
			t.Fatalf("wrong final head: %+v", result)
		}
	}
}

func TestAutomaticShipmentExpiry(t *testing.T) {
	for _, name := range []string{"all merged", "second open", "closed unmerged", "API failure", "recent merge", "pagination failure", "new work", "changed HEAD", "merge during archive"} {
		t.Run(name, func(t *testing.T) {
			s, task, checks := expiryFixture(t)
			gitTest(t, task.Repo, "remote", "add", "origin", "https://github.com/owner/repo.git")
			if err := s.Store.UpdateGitHub(func(v *GitHubState) error {
				v.Identities = map[string]GitHubIdentity{"fixture": {Mode: "gh", Login: "engineer"}}
				v.Routes = map[string]string{"owner/repo": "fixture"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			merged := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
			if name == "recent merge" {
				merged = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
			}
			sha := task.ShippedCommit
			if name == "changed HEAD" {
				sha = strings.Repeat("f", 40)
			}
			p := PullRequest{Number: 1, State: "closed", Merged: true, MergedAt: merged}
			p.Base.Ref = "main"
			p.Base.Repo.FullName = "owner/repo"
			p.Head.Ref = task.Branch
			p.Head.Repo.FullName = "owner/repo"
			p.Head.SHA = sha
			q := p
			q.Number = 2
			if name == "second open" {
				q.State = "open"
				q.Merged = false
			}
			if name == "closed unmerged" {
				q.Merged = false
			}
			if err := s.Store.update(task.ID, func(v *Task) error {
				v.ShippedAt = ""
				v.ShippedCommit = ""
				v.LastWorkAt = time.Now().Add(-96 * time.Hour).UTC().Format(time.RFC3339Nano)
				if name == "new work" {
					v.LastWorkAt = now()
				}
				v.PullRequests = []TaskPullRequest{{"owner/repo", 2, "owner/repo", task.Branch}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			data, _ := json.Marshal(map[string]any{"mode": name, "pr": p, "other": q})
			fixture := filepath.Join(dir, "fixture.json")
			if err := os.WriteFile(fixture, data, 0600); err != nil {
				t.Fatal(err)
			}
			script := `#!/usr/bin/env python3
import json,sys,os,datetime
x=json.load(open(` + strconv.Quote(fixture) + `))
p=next(a for a in sys.argv if a=='user' or a.startswith('repos/'))
if x['mode']=='merge during archive':
 counter=__file__+'.count'
 n=int(open(counter).read()) if os.path.exists(counter) else 0
 if '/pulls?' in p:
  n+=1
  open(counter,'w').write(str(n))
 if n>=3:x['pr']['merged_at']=datetime.datetime.now(datetime.timezone.utc).isoformat()
if x['mode']=='API failure':sys.exit(1)
if p=='user':out={'login':'engineer'}
elif p=='repos/owner/repo':out={'full_name':'owner/repo'}
elif '/pulls?' in p:
 if x['mode']=='pagination failure':
  if 'page=2' in p:sys.exit(1)
  out=[x['pr']]*100
 else:out=[x['pr'],x['other']]
elif p.endswith('/pulls/1'):out=x['pr']
elif p.endswith('/pulls/2'):out=x['other']
else:sys.exit(2)
print(json.dumps(out))
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			r, err := s.expireTask(context.Background(), task.ID, true, checks)
			if err != nil {
				t.Fatal(err)
			}
			if name == "all merged" {
				if !r.Removed {
					t.Fatalf("merged work retained: %+v", r)
				}
				saved, _ := s.Store.Get(task.ID)
				if saved.ShipmentSource != "github" || saved.Archive == nil {
					t.Fatalf("missing durable evidence: %+v", saved)
				}
			} else {
				if r.Removed || r.Eligible {
					t.Fatalf("unsafe expiry: %+v", r)
				}
				if _, e := os.Stat(task.Worktree); e != nil {
					t.Fatal("unsafe checkout removal", e)
				}
			}
		})
	}
}

func TestWatchRemovalPreservesShipmentInventory(t *testing.T) {
	g, _, store, w, _ := githubFixture(t)
	repo := repoFixture(t)
	gitTest(t, repo, "remote", "add", "origin", "https://github.com/team/repo.git")
	if err := store.update(w.TaskID, func(v *Task) error { v.Worktree = repo; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Watch(context.Background(), "https://github.com/team/repo/pull/1", w.TaskID, []string{"engineer"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateGitHub(func(v *GitHubState) error { delete(v.Watches, w.Key()); return nil }); err != nil {
		t.Fatal(err)
	}
	task, _ := store.Get(w.TaskID)
	if len(task.PullRequests) != 1 || task.PullRequests[0].Number != 1 {
		t.Fatalf("lost shipment association: %+v", task.PullRequests)
	}
}
func TestWorkClockDistinguishesExitAndNewWork(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.LastWorkAt = "2026-01-01T00:00:00Z"
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(task.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.Get(task.ID)
	if saved.LastWorkAt != task.LastWorkAt {
		t.Fatal("exit invalidated shipment")
	}
	if err := store.TouchActivity(task.ID); err != nil {
		t.Fatal(err)
	}
	saved, _ = store.Get(task.ID)
	if saved.LastWorkAt == task.LastWorkAt || saved.LastWorkAt != saved.LastActivityAt {
		t.Fatal("new work failed to invalidate shipment")
	}
}
