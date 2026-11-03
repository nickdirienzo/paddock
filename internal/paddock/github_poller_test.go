package paddock

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSharedPollerLifecycle(t *testing.T) {
	exe := buildCLI(t)
	s := openTestStore(t, t.TempDir())
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(exe, append([]string{"github"}, args...)...)
		cmd.Env = append(os.Environ(), "PADDOCK_HOME="+s.Home)
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() {
		if b, e := run("stop"); e != nil {
			t.Errorf("cleanup: %s %v", b, e)
		}
	})
	// Distinct launching processes race to register; only one child may survive.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if b, e := run("start", "--interval", "5s"); e != nil {
				t.Errorf("start: %s %v", b, e)
			}
		})
	}
	wg.Wait()
	first, err := s.GitHubPollerStatus()
	if err != nil || !first.Running || first.Mode != "background" || first.PID == os.Getpid() {
		t.Fatalf("status: %+v %v", first, err)
	}
	// All launchers have exited; service is still reachable and reused.
	if b, e := run("start"); e != nil {
		t.Fatalf("reuse: %s %v", b, e)
	}
	same, _ := s.GitHubPollerStatus()
	if same.Instance != first.Instance || same.PID != first.PID {
		t.Fatal("started a duplicate")
	}
	if b, e := run("poll", "--interval", "5s"); e == nil || !strings.Contains(string(b), "already running") {
		t.Fatalf("foreground lease: %s %v", b, e)
	}
	// A stale stop request cannot stop a newer instance.
	if err := writeJSONAtomic(s.pollerFile("-stop.json"), "old-instance"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if status, _ := s.GitHubPollerStatus(); !status.Running {
		t.Fatal("stale stop killed current poller")
	}
	if b, e := run("status", "--json"); e != nil {
		t.Fatalf("status: %s %v", b, e)
	} else {
		var state struct {
			Poller GitHubPollerStatus `json:"poller"`
		}
		if json.Unmarshal(b, &state) != nil || !state.Poller.Running {
			t.Fatalf("CLI status: %s", b)
		}
	}
	if b, e := run("stop"); e != nil {
		t.Fatalf("stop: %s %v", b, e)
	}
	stopped, err := s.GitHubPollerStatus()
	if err != nil || stopped.Running {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if b, e := run("start", "--interval", "5s"); e != nil {
		t.Fatalf("restart: %s %v", b, e)
	}
	restarted, _ := s.GitHubPollerStatus()
	if !restarted.Running || restarted.Instance == first.Instance {
		t.Fatal("restart reused old instance")
	}
}

func TestPollLeavesOfflineCommentsPending(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted", "unknown", "missing"} {
		t.Run(status, func(t *testing.T) {
			g, remote, s, watch, log := githubFixture(t)
			remote.comments = []GitHubComment{testComment(1, "please fix", 7)}
			task, _ := s.Get(watch.TaskID)
			if status == "unknown" || status == "missing" {
				body := "exit 2\n"
				if status == "missing" {
					body = "echo 'No Sockets found in /tmp/screen.'; exit 1\n"
				}
				if err := os.WriteFile(task.Session.TerminalPath, []byte("#!/bin/sh\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := s.Status(task.ID, status, ""); err != nil {
				t.Fatal(err)
			}
			if events, err := g.Poll(context.Background()); err != nil || len(events) != 0 {
				t.Fatalf("offline: %+v %v", events, err)
			}
			state, _ := s.GitHubState()
			if len(state.Events) != 0 || remote.reactionCalls != 0 {
				t.Fatal("offline comment consumed or acknowledged")
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("offline agent received message")
			}
			current, _ := s.Get(task.ID)
			if status == "missing" && current.Status != "interrupted" {
				t.Fatal("missing session not refreshed")
			}
			if status == "unknown" && current.Status != "running" {
				t.Fatal("unknown session permanently marked offline")
			}
			// Once the same registered task is running again, the original comment is delivered.
			if err := os.WriteFile(task.Session.TerminalPath, []byte("#!/bin/sh\nprintf '123."+task.Session.Name+" (Detached)\\n'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := s.Status(task.ID, "running", ""); err != nil {
				t.Fatal(err)
			}
			if events, err := g.Poll(context.Background()); err != nil || len(events) != 1 || events[0].Status != "done" {
				t.Fatalf("resume: %+v %v", events, err)
			}
			if _, err := g.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(log)
			if strings.Count(string(data), "queue --thread") != 1 || remote.reactionCalls != 1 {
				t.Fatal("resumed comment duplicated")
			}
		})
	}
}

func TestPollExitBetweenCheckAndMessage(t *testing.T) {
	g, remote, s, w, log := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "please fix", 7)}
	task, _ := s.Get(w.TaskID)
	counter := filepath.Join(t.TempDir(), "inspected")
	body := "#!/bin/sh\nif test -f " + quote(counter) + "; then echo 'No Sockets found in /tmp/screen.'; exit 1; fi\ntouch " + quote(counter) + "\nprintf '123." + task.Session.Name + " (Detached)\\n'\n"
	if err := os.WriteFile(task.Session.TerminalPath, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 0 {
		t.Fatalf("exit race: %+v %v", events, err)
	}
	state, _ := s.GitHubState()
	event := state.Events[githubEventID(w, remote.comments[0])]
	if event.Status != "pending" || remote.reactionCalls != 0 {
		t.Fatal("offline action must remain pending without acknowledgement")
	}
	record, err := s.SignalRecord("github", event.ID)
	if err != nil || record.Status != "pending" {
		t.Fatalf("unattempted action not retryable: %+v %v", record, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("message attempted")
	}
}

func TestOfflineWatchDoesNotBlockOthersOrReplies(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "please fix", 7)}
	offline := taskFixture()
	offline.ID = newID()
	offline.Status = "completed"
	if err := s.Insert(offline); err != nil {
		t.Fatal(err)
	}
	other := w
	other.Repo = "aaa/offline"
	other.TaskID = offline.ID
	if err := s.UpdateGitHub(func(v *GitHubState) error { v.Watches[other.Key()] = other; return nil }); err != nil {
		t.Fatal(err)
	}
	events, err := g.Poll(context.Background())
	if err != nil || len(events) != 1 || events[0].TaskID != w.TaskID {
		t.Fatalf("active delivery: %+v %v", events, err)
	}
	if _, err := g.EnqueueReply(events[0].ID, "Fixed and tested."); err != nil {
		t.Fatal(err)
	}
	if err := s.Status(w.TaskID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.replies) != 1 {
		t.Fatal("exited agent's queued reply was stranded")
	}
}

func TestPollerRetriesErrorsAndCancels(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "please fix", 7)}
	// First pass fails ownership validation; repair it after observing that error.
	remote.pr.Head.Ref = "changed-branch"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	passes := 0
	err := g.RunGitHubPoller(ctx, 5*time.Second, func(events []GitHubEvent, pollErr error) error {
		passes++
		status, err := s.GitHubPollerStatus()
		if err != nil || !status.Running || status.LastPoll == "" {
			t.Fatalf("health: %+v %v", status, err)
		}
		if passes == 1 {
			if pollErr == nil || status.LastError == "" {
				t.Fatal("remote error not recorded")
			}
			remote.pr.Head.Ref = w.HeadRef
		} else {
			if pollErr != nil || status.LastError != "" || len(events) != 1 {
				t.Fatalf("retry: %+v %v", events, pollErr)
			}
			cancel()
		}
		return nil
	})
	if err != nil || passes != 2 {
		t.Fatalf("passes=%d err=%v", passes, err)
	}
	if status, _ := s.GitHubPollerStatus(); status.Running {
		t.Fatal("lease leaked after cancellation")
	}
}

func TestPollBrokenWatchDoesNotBlockHealthyWatch(t *testing.T) {
	g, remote, s, w, _ := githubFixture(t)
	remote.comments = []GitHubComment{testComment(1, "please fix", 7)}
	broken := w
	broken.Repo = "aaa/broken"
	if err := s.UpdateGitHub(func(v *GitHubState) error { v.Watches[broken.Key()] = broken; return nil }); err != nil {
		t.Fatal(err)
	}
	events, err := g.Poll(context.Background())
	if err == nil || len(events) != 1 || events[0].Receipt != "posted" {
		t.Fatalf("healthy watch starved: %+v %v", events, err)
	}
}
