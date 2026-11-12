package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func taskFixture() Task {
	return Task{ID: newID(), Prompt: "test", Status: "starting", CreatedAt: now(), UpdatedAt: now(), Worktree: "/tmp/work", Session: Session{Agent: "codex", Backend: "screen"}}
}
func openTestStore(t *testing.T, home string) *Store {
	t.Helper()
	s, e := OpenStore(home)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestNativeTurnSnapshotSurvivesSessionExitAndRegistryReopen(t *testing.T) {
	home := t.TempDir()
	store := openTestStore(t, home)
	task := taskFixture()
	task.Session.Generation = 2
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	turn := AgentTurn{ID: "turn-1", State: "completed"}
	if err := store.UpdateSession(task.ID, func(session *Session) { session.LastTurn = &turn }); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(task.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, home)
	got, err := reopened.Get(task.ID)
	if err != nil || got.Status != "stopped" || got.Session.Generation != 2 || got.Session.LastTurn == nil || *got.Session.LastTurn != turn {
		t.Fatalf("native turn snapshot lost after exit: %+v, %v", got, err)
	}
}
func TestStorePersistenceAndRelationships(t *testing.T) {
	home := t.TempDir()
	s := openTestStore(t, home)
	parent := taskFixture()
	child := taskFixture()
	child.ParentID = parent.ID
	if e := s.Insert(child); e == nil {
		t.Fatal("accepted missing parent")
	}
	for _, v := range []Task{parent, child} {
		if e := s.Insert(v); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Insert(parent); e == nil {
		t.Fatal("accepted duplicate")
	}
	s2 := openTestStore(t, home)
	got, e := s2.Get(child.ID[:8])
	if e != nil {
		t.Fatal(e)
	}
	if got.ParentID != parent.ID {
		t.Fatal(got)
	}
	children, e := s2.List(parent.ID)
	if e != nil || len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("%+v %v", children, e)
	}
	if e = s2.Claim(child.ID); e != nil {
		t.Fatal(e)
	}
	if e = s2.Claim(child.ID); e == nil {
		t.Fatal("accepted duplicate supervisor")
	}
	if e = s2.Finish(child.ID, 0, ""); e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateSession(child.ID, func(v *Session) { v.ID = "terminal-id" }); e != nil {
		t.Fatal(e)
	}
	got, e = s.Get(child.ID)
	if e != nil || got.Status != "stopped" || got.Session.ExitCode == nil || *got.Session.ExitCode != 0 || got.Session.ID != "terminal-id" {
		t.Fatalf("%+v %v", got, e)
	}
}
func TestStoreCorruptAndAmbiguous(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	a := taskFixture()
	a.ID = strings.Repeat("a", 24)
	b := a
	b.ID = strings.Repeat("a", 23) + "b"
	if e := s.Insert(a); e != nil {
		t.Fatal(e)
	}
	if e := s.Insert(b); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Get("aaa"); e == nil {
		t.Fatal("accepted ambiguous prefix")
	}
	if e := os.WriteFile(s.path(a.ID), []byte(`{"version":999}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Get(a.ID); e == nil || !strings.Contains(e.Error(), "unsupported") {
		t.Fatal(e)
	}
	if _, e := s.List(""); e == nil {
		t.Fatal("silently ignored corrupt record")
	}
}

func TestStoreHelper(t *testing.T) {
	if os.Getenv("PADDOCK_STORE_HELPER") == "" {
		return
	}
	s, e := OpenStore(os.Getenv("PADDOCK_STORE_TEST_HOME"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if name := os.Getenv("PADDOCK_REPO_TEST_NAME"); name != "" {
		if _, err := s.AddRepo(context.Background(), name, os.Getenv("PADDOCK_REPO_TEST_PATH")); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Getenv("PADDOCK_LOCK_HELPER") == "1" {
		e = s.withLock(func() error {
			if e := os.WriteFile(filepath.Join(s.Home, "locked"), nil, 0600); e != nil {
				return e
			}
			select {}
		})
		if e != nil {
			t.Fatal(e)
		}
		return
	}
	for i := 0; i < 10; i++ {
		if e := s.update(os.Getenv("PADDOCK_STORE_TEST_ID"), func(v *Task) error { n, _ := strconv.Atoi(v.Prompt); v.Prompt = strconv.Itoa(n + 1); return nil }); e != nil {
			t.Fatal(e)
		}
	}
}
func TestConcurrentProcessesAndAtomicReaders(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	v := taskFixture()
	v.Prompt = "0"
	if e := s.Insert(v); e != nil {
		t.Fatal(e)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			b, e := os.ReadFile(s.path(v.ID))
			if e != nil {
				done <- e
				return
			}
			var r record
			if e = json.Unmarshal(b, &r); e != nil {
				done <- e
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=^TestStoreHelper$")
			c.Env = append(os.Environ(), "PADDOCK_STORE_HELPER=1", "PADDOCK_STORE_TEST_HOME="+s.Home, "PADDOCK_STORE_TEST_ID="+v.ID)
			if b, e := c.CombinedOutput(); e != nil {
				t.Errorf("%v: %s", e, b)
			}
		}()
	}
	wg.Wait()
	close(stop)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	got, e := s.Get(v.ID)
	if e != nil || got.Prompt != "60" {
		t.Fatalf("lost update: %+v %v", got, e)
	}
}
func TestLockReleasedAfterProcessDeath(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	c := exec.Command(os.Args[0], "-test.run=^TestStoreHelper$")
	c.Env = append(os.Environ(), "PADDOCK_STORE_HELPER=1", "PADDOCK_LOCK_HELPER=1", "PADDOCK_STORE_TEST_HOME="+s.Home)
	if e := c.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(s.Home, "locked")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e := c.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = c.Wait()
	if e := s.Insert(taskFixture()); e != nil {
		t.Fatal("lock survived process death:", e)
	}
}
