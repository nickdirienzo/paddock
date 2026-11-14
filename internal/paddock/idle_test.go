package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdleTTLValidation(t *testing.T) {
	for _, value := range []string{"0", "0s", "-1h", "24", "1d", "forever", "999999999999999999h", " "} {
		if _, err := NormalizeIdleTTL(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
		s := Service{Store: openTestStore(t, t.TempDir())}
		if _, err := s.Run(context.Background(), RunOptions{Prompt: "task", IdleTTL: value}); err == nil || !strings.Contains(err.Error(), "idle-ttl") {
			t.Fatal(value, err)
		}
	}
	for value, want := range map[string]string{"": "", "24h": "24h0m0s", "1440m": "24h0m0s", "1ns": "1ns"} {
		got, err := NormalizeIdleTTL(value)
		if err != nil || got != want {
			t.Fatal(value, got, err)
		}
	}
}

func idleFixture(t *testing.T) (Service, Task, time.Time) {
	t.Helper()
	service := Service{Store: openTestStore(t, t.TempDir())}
	task := taskFixture()
	task.Runtime, task.Status, task.IdleTTL = "openshell", "running", "1h0m0s"
	task.Session.Agent, task.Session.Generation, task.Session.ConversationID = "codex", 1, newConversationID()
	task.LastActivityAt = "2020-01-01T00:00:00Z"
	if err := service.Store.Insert(task); err != nil {
		t.Fatal(err)
	}
	return service, task, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
}

func idleTestChecks(t *testing.T, state *idleRuntimeState, expired *int) idleChecks {
	return idleChecks{
		runtime: func(_ context.Context, task Task, operation string) (idleRuntimeState, error) {
			if operation == "expire" {
				*expired++
				if task.Idle.Phase != "releasing" {
					t.Fatal("shutdown intent not saved", task.Idle)
				}
				return idleRuntimeState{State: "draining", Epoch: state.Epoch, TurnID: state.TurnID}, nil
			}
			return *state, nil
		},
		output: func(context.Context, Task) ([]OutputEvent, error) {
			return []OutputEvent{{ID: "m:answer:0", TurnID: "turn", Type: "message", Text: "saved answer"}}, nil
		},
		finish: func(context.Context, Task) error { return nil },
	}
}

func TestIdleCountdownPollingRestartAndExpiry(t *testing.T) {
	s, task, clock := idleFixture(t)
	native, expired := idleRuntimeState{State: "completed", Epoch: "runtime-1", TurnID: "turn"}, 0
	checks := idleTestChecks(t, &native, &expired)
	pass := func(clock time.Time) Task {
		t.Helper()
		if err := s.reconcileIdle(context.Background(), clock, checks); err != nil {
			t.Fatal(err)
		}
		got, err := s.Store.Get(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := pass(clock)
	if got.Idle.Since != clock.Format(time.RFC3339Nano) || expired != 0 || got.LastActivityAt != task.LastActivityAt {
		t.Fatal(got)
	}
	deadline := got.Idle.Deadline
	got = pass(clock.Add(30 * time.Minute))
	if got.Idle.Deadline != deadline || expired != 0 {
		t.Fatal("polling changed deadline", got.Idle)
	}
	// Another Store/Service instance reads the durable clock after a restart.
	s = Service{Store: openTestStore(t, s.Store.Home)}
	got = pass(clock.Add(time.Hour))
	if expired != 1 || got.Idle.Phase != "draining" {
		t.Fatal(expired, got.Idle)
	}
	got = pass(clock.Add(2 * time.Hour))
	if expired != 1 {
		t.Fatal("repeated shutdown was sent")
	}
	page, err := s.Output(context.Background(), got, "", 20)
	if err != nil || len(page.Events) != 1 || page.Events[0].Text != "saved answer" {
		t.Fatal(page, err)
	}
}

func TestIdleActivityAndUnknownRestartTheCountdown(t *testing.T) {
	for _, state := range []string{"active", "approval", "input", "unknown", "none", "failed", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			s, task, clock := idleFixture(t)
			native, expired := idleRuntimeState{State: "completed", Epoch: "epoch", TurnID: "turn"}, 0
			checks := idleTestChecks(t, &native, &expired)
			for _, step := range []struct {
				offset time.Duration
				state  string
			}{{0, "completed"}, {2 * time.Hour, state}, {3 * time.Hour, "completed"}} {
				native.State = step.state
				if err := s.reconcileIdle(context.Background(), clock.Add(step.offset), checks); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := s.Store.Get(task.ID)
			if expired != 0 || got.Idle.Since != clock.Add(3*time.Hour).Format(time.RFC3339Nano) {
				t.Fatal(expired, got.Idle)
			}
		})
	}
	t.Run("same turn but new admitted work epoch", func(t *testing.T) {
		s, task, clock := idleFixture(t)
		native, expired := idleRuntimeState{State: "completed", Epoch: "epoch", TurnID: "turn"}, 0
		checks := idleTestChecks(t, &native, &expired)
		_ = s.reconcileIdle(context.Background(), clock, checks)
		native.Epoch = "new request"
		_ = s.reconcileIdle(context.Background(), clock.Add(2*time.Hour), checks)
		got, _ := s.Store.Get(task.ID)
		if expired != 0 || got.Idle.Since != clock.Add(2*time.Hour).Format(time.RFC3339Nano) {
			t.Fatal(got.Idle)
		}
	})
}

func TestIdlePendingWorkAndDeliverySerialize(t *testing.T) {
	for _, pending := range []string{"operation lock", "pending receipt", "uncertain receipt", "claimed receipt", "access"} {
		t.Run(pending, func(t *testing.T) {
			s, task, clock := idleFixture(t)
			native, expired := idleRuntimeState{State: "completed", Epoch: "epoch", TurnID: "turn"}, 0
			checks := idleTestChecks(t, &native, &expired)
			if err := s.reconcileIdle(context.Background(), clock, checks); err != nil {
				t.Fatal(err)
			}
			switch pending {
			case "operation lock":
				release, err := s.Store.taskLease(task.ID, "operation")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "access":
				// Existing noPendingWork handles GitHub signals and access proposals too.
				receipt := SignalRecord{Status: "uncertain", Signal: Signal{TaskID: task.ID}}
				if err := os.MkdirAll(filepath.Join(s.Store.Home, "signals"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := writeJSONAtomic(filepath.Join(s.Store.Home, "signals", "fixture.json"), receipt); err != nil {
					t.Fatal(err)
				}
			default:
				status := strings.Fields(pending)[0]
				if err := writeJSONAtomic(filepath.Join(s.Store.Home, "messages", "fixture.json"), MessageReceipt{TaskID: task.ID, Status: status}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.reconcileIdle(context.Background(), clock.Add(2*time.Hour), checks); err != nil {
				t.Fatal(err)
			}
			if expired != 0 {
				t.Fatal("expired with pending work")
			}
		})
	}
	t.Run("message claim cannot race expiry operation", func(t *testing.T) {
		s, task, _ := idleFixture(t)
		release, err := s.Store.taskLease(task.ID, "operation")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, _, err := s.Store.ClaimMessage(task.ID, "caller", "key", "work"); !errors.Is(err, ErrAgentOffline) {
			t.Fatal(err)
		}
	})
}

func TestIdleNoDefaultNoInheritanceAndUnsupported(t *testing.T) {
	for _, variant := range []string{"no policy", "claude"} {
		t.Run(variant, func(t *testing.T) {
			s, task, clock := idleFixture(t)
			_ = s.Store.update(task.ID, func(v *Task) error {
				if variant == "no policy" {
					v.IdleTTL = ""
				} else {
					v.Session.Agent = "claude"
				}
				return nil
			})
			checks := idleChecks{runtime: func(context.Context, Task, string) (idleRuntimeState, error) {
				t.Fatal("unsupported runtime was called")
				return idleRuntimeState{}, nil
			}}
			if err := s.reconcileIdle(context.Background(), clock, checks); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Store.Get(task.ID)
			if variant == "claude" && (got.Idle == nil || got.Idle.Phase != "unsupported") {
				t.Fatal(got.Idle)
			}
			if variant == "no policy" && got.Idle != nil {
				t.Fatal(got.Idle)
			}
		})
	}
	// RunOptions deliberately has no inferred parent policy: the child default is
	// empty even if the parent has a TTL. Assert on an idempotent existing child.
	s, parent, _ := idleFixture(t)
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	child := Task{ID: newID(), ParentID: parent.ID, Runtime: "openshell", Prompt: "child", Session: Session{Agent: "codex"}, Provenance: TaskProvenance{Caller: caller, ExternalID: "child", Profile: "implementation"}}
	if err := s.Store.Insert(child); err != nil {
		t.Fatal(err)
	}
	got, err := s.Run(context.Background(), RunOptions{Parent: parent.ID, Prompt: "child", ExternalID: "child"})
	if err != nil || got.IdleTTL != "" {
		t.Fatal(got, err)
	}
}

func TestIdleDispatchPolicyIsIdempotentAndImmutable(t *testing.T) {
	s, task, _ := idleFixture(t)
	caller, err := LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	// Provenance is immutable, so insert another dispatch fixture.
	task.ID = newID()
	task.IdleTTL = "24h0m0s"
	task.Prompt = "work"
	task.Provenance = TaskProvenance{Caller: caller, ExternalID: "dispatch", Profile: "implementation"}
	if err := s.Store.Insert(task); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"24h", "1440m", "", "12h"} {
		got, err := s.Run(context.Background(), RunOptions{Prompt: "work", ExternalID: "dispatch", IdleTTL: value})
		if value == "24h" || value == "1440m" {
			if err != nil || got.ID != task.ID {
				t.Fatal(got, err)
			}
		} else if err == nil {
			t.Fatal("changed/omitted policy accepted", value)
		}
	}
	saved, _ := s.Store.Get(task.ID)
	if saved.IdleTTL != "24h0m0s" {
		t.Fatal(saved.IdleTTL)
	}
}

func TestIdleRejectsChangedEpochAndUncertainRelease(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		s, task, clock := idleFixture(t)
		native, expired := idleRuntimeState{State: "completed", Epoch: "epoch", TurnID: "turn"}, 0
		checks := idleTestChecks(t, &native, &expired)
		_ = s.reconcileIdle(context.Background(), clock, checks)
		checks.runtime = func(_ context.Context, _ Task, op string) (idleRuntimeState, error) {
			if op == "expire" {
				if uncertain {
					return idleRuntimeState{}, errors.New("lost response")
				}
				return idleRuntimeState{State: "unknown"}, nil
			}
			return native, nil
		}
		err := s.reconcileIdle(context.Background(), clock.Add(time.Hour), checks)
		got, _ := s.Store.Get(task.ID)
		if uncertain {
			if err == nil || got.Idle.Phase != "releasing" {
				t.Fatal(err, got.Idle)
			}
		} else if err != nil || got.Idle.Phase != "waiting" {
			t.Fatal(err, got.Idle)
		}
		// A rejected/uncertain checkpoint must not hide later output behind its cache.
		if got.Idle.Phase == "draining" {
			t.Fatal(got.Idle)
		}
	}
}

func TestIdleFinishRequiresProofAndClosesOnlyOwnedPane(t *testing.T) {
	for _, proof := range []string{"correct", "stale generation", "wrong epoch"} {
		t.Run(proof, func(t *testing.T) {
			s, task, _ := idleFixture(t)
			task.Idle = &IdleState{Generation: 1, Phase: "draining", Epoch: "epoch", TurnID: "turn"}
			log := filepath.Join(t.TempDir(), "calls")
			marker := map[string]any{"generation": 1, "thread": task.Session.ConversationID, "epoch": "epoch", "turn_id": "turn"}
			if proof == "stale generation" {
				marker["generation"] = 0
			}
			if proof == "wrong epoch" {
				marker["epoch"] = "other"
			}
			data, _ := json.Marshal(marker)
			task.OpenShell = &OpenShellTask{Name: "pd-fixture", Config: OpenShellConfig{Gateway: "fixture", Workspace: "default"}}
			task.OpenShell.Config.Binary = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'sandbox get '*) printf '%s' '{"name":"pd-fixture","phase":"Ready","labels":{"paddock.task":"`+task.ID+`"}}';;
 *'cat /sandbox/idle-stopped.json'*) printf '%s' `+quote(string(data))+`;;
 *'cat /sandbox/agent-exit.json'*) printf '%s' '{"generation":1,"code":0}';;
 *'sandbox stop '*) exit 0;;
 *) printf '%s' '{"state":"unknown"}';;
esac`)
			task.Session.Backend, task.Session.ID, task.Session.BlockID = "rex", "session:family", "block:owned"
			task.Session.TerminalPath = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:family"}]}';;
 *'process') printf '%s' '{"child":null,"last_exit":{"exit_code":0}}';;
 *'block close -s session:family block:owned') exit 0;;
 *) exit 1;;
esac`)
			_ = s.Store.update(task.ID, func(v *Task) error { *v = task; return nil })
			if proof == "correct" {
				release, err := s.Store.taskLease(task.ID, "process")
				if err != nil {
					t.Fatal(err)
				}
				if err := s.finishIdle(context.Background(), task); err != nil {
					t.Fatal(err)
				}
				calls, _ := os.ReadFile(log)
				if strings.Contains(string(calls), "sandbox stop") || strings.Contains(string(calls), "block close") {
					t.Fatal("cleanup raced live supervisor", string(calls))
				}
				release()
				task, _ = s.Store.Get(task.ID)
			}
			err := s.finishIdle(context.Background(), task)
			calls, _ := os.ReadFile(log)
			if proof != "correct" {
				if err == nil || strings.Contains(string(calls), "block close") || strings.Contains(string(calls), "sandbox stop") {
					t.Fatal(err, string(calls))
				}
				return
			}
			if err != nil {
				t.Fatal(err, string(calls))
			}
			saved, _ := s.Store.Get(task.ID)
			if saved.Status != "stopped" || saved.Idle.Phase != "expired" || !saved.Idle.PaneClosed || !saved.Idle.DrainConfirmed {
				t.Fatal(saved)
			}
			if !strings.Contains(string(calls), "block close -s session:family block:owned") || strings.Contains(string(calls), "window close") || strings.Contains(string(calls), "kill session:") {
				t.Fatal(string(calls))
			}
		})
	}
}

func TestIdleFollowupMessageResumesPinnedConversation(t *testing.T) {
	s, task, _ := idleFixture(t)
	task.Status = "stopped"
	task.Worktree = "/sandbox/worktree"
	task.Prompt = "ORIGINAL ASSIGNMENT MUST NOT REPLAY"
	task.Session.AgentPath = "/usr/local/bin/codex"
	task.Session.AgentHome = "/home/sandbox/.codex"
	task.Session.Backend = "rex"
	task.Session.ID = "session:family"
	task.Session.BlockID = "block:old"
	task.Session.WindowID = "window:old"
	task.Session.ExitCode = new(int)
	task.Idle = &IdleState{Generation: 1, Phase: "expired", PaneClosed: true, DrainConfirmed: true}
	log, active := filepath.Join(t.TempDir(), "calls"), filepath.Join(t.TempDir(), "active")
	task.OpenShell = &OpenShellTask{Name: "pd-fixture", Ready: true, Config: OpenShellConfig{Gateway: "fixture", Workspace: "default"}}
	task.OpenShell.Config.Binary = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'sandbox get '*) printf '%s' '{"name":"pd-fixture","phase":"Stopped","labels":{"paddock.task":"`+task.ID+`"}}';;
 *'app-server --stdio')
  while IFS= read -r line; do
   case "$line" in
    *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}';;
    *'"id":2'*) printf '%s\n' '{"id":2,"result":{"thread":{"id":"`+task.Session.ConversationID+`","cwd":"/sandbox/worktree"}}}';;
   esac
  done;;
 *'lexists'*) printf present;;
 *'rev-parse --show-toplevel') printf /sandbox/worktree;;
 *'rev-parse --path-format=absolute --git-common-dir') printf /sandbox/repository.git;;
 *'symbolic-ref --quiet HEAD') printf '%s' `+quote("refs/heads/"+task.Branch)+`;;
 *'paddock-codex-state owner '*) printf '{"owner":true}';;
 *'paddock-codex-state start-queued '*) printf '{"owner":true}';;
 *) exit 0;;
esac`)
	// Simulate the terminal adapter's supervisor claim without starting any real
	// agent: actual native lifecycle behavior is covered by test_idle_native.py.
	claim := `import json,os,sys
p=sys.argv[1]
v=json.load(open(p));v['status']='running'
q=p+'.fixture';json.dump(v,open(q,'w'));os.replace(q,p)`
	task.Session.TerminalPath = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:family"}]}';;
 *'process') if [ -f `+quote(active)+` ]; then printf '%s' '{"child":{"pid":42}}'; else printf '%s' '{"child":null,"last_exit":{"exit_code":0}}'; fi;;
 *'block close '*) exit 0;;
 *'window new '*)
  touch `+quote(active)+`
  python3 -c `+quote(claim)+` `+quote(s.Store.path(task.ID))+`
  printf '%s' '{"window_id":"window:new","block_ids":["block:new"]}';;
 *) exit 1;;
esac`)
	if err := s.Store.update(task.ID, func(v *Task) error { *v = task; return nil }); err != nil {
		t.Fatal(err)
	}
	poller, locked, err := s.Store.pollerLock("", false)
	if err != nil || !locked {
		t.Fatal(err)
	}
	defer releasePollerLock(poller)
	if err := writeJSONAtomic(s.Store.pollerFile(".json"), GitHubPollerStatus{RuntimeVersion: githubPollerVersion}); err != nil {
		t.Fatal(err)
	}
	thread, err := s.Message(context.Background(), task, "FOLLOWUP ONLY")
	if err != nil || thread != task.Session.ConversationID {
		t.Fatal(thread, err)
	}
	saved, err := s.Store.Get(task.ID)
	if err != nil || saved.ID != task.ID || saved.Branch != task.Branch || saved.Worktree != task.Worktree || saved.Session.ConversationID != task.Session.ConversationID || saved.Session.Generation != 2 || !saved.Session.Resume || saved.Idle != nil || saved.IdleTTL != task.IdleTTL {
		t.Fatal(saved, err)
	}
	command := AgentCommand(saved, s.Store.Home, "/fixture/paddock")
	args := strings.Join(command.Args, " ")
	if !strings.Contains(args, "resume "+task.Session.ConversationID) || strings.Contains(args, task.Prompt) {
		t.Fatal(args)
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "queue --thread "+task.Session.ConversationID+" --message FOLLOWUP ONLY") != 1 || strings.Contains(string(calls), task.Prompt) || !strings.Contains(string(calls), "--remote unix:///sandbox/idle-rpc.sock") {
		t.Fatal(string(calls))
	}
}

func TestIdleConfirmationClockAndRemovedPaneWithUserSplit(t *testing.T) {
	s, task, clock := idleFixture(t)
	native, expired := idleRuntimeState{State: "completed", Epoch: "epoch", TurnID: "turn"}, 0
	checks := idleTestChecks(t, &native, &expired)
	checks.now = func() time.Time { return clock.Add(2 * time.Hour) }
	if err := s.reconcileIdle(context.Background(), clock, checks); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.Get(task.ID)
	if saved.Idle.Since != clock.Add(2*time.Hour).Format(time.RFC3339Nano) || expired != 0 {
		t.Fatal(saved.Idle)
	}
	task.Session.Backend, task.Session.ID, task.Session.BlockID = "rex", "session:family", "block:removed"
	task.Idle = &IdleState{Generation: task.Session.Generation, DrainConfirmed: true, Phase: "draining"}
	task.Session.TerminalPath = script(t, `case "$*" in
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:family"}]}';;
 *'process') exit 1;;
 *'block ls '*) printf '%s' '{"blocks":[{"block_id":"block:user-added"},{"block_id":"block:sibling"}]}';;
 *) exit 1;;
esac`)
	if err := closeExitedTerminal(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task.Idle.DrainConfirmed = false
	if err := closeExitedTerminal(context.Background(), task); err == nil {
		t.Fatal("missing block was treated as a safe exit without native proof")
	}
}

func TestIdleUnknownLiveAccessRetainsAndClearsDeadline(t *testing.T) {
	s, task, clock := idleFixture(t)
	native, expired := idleRuntimeState{State: "completed", Epoch: "epoch", TurnID: "turn"}, 0
	checks := idleTestChecks(t, &native, &expired)
	if err := s.reconcileIdle(context.Background(), clock, checks); err != nil {
		t.Fatal(err)
	}
	checks.access = func(context.Context, Task) error { return errors.New("gateway unavailable") }
	if err := s.reconcileIdle(context.Background(), clock.Add(time.Hour), checks); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.Get(task.ID)
	if expired != 0 || saved.Idle.Deadline != "" || !strings.Contains(saved.Idle.Error, "live access state is unknown") {
		t.Fatal(saved.Idle)
	}
}
