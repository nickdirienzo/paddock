//go:build darwin || linux

package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func hostClaudeFixture(t *testing.T, task Task, admission string) (Task, claudeInbox, <-chan string) {
	t.Helper()
	task.Runtime = "host"
	task.Session.Agent = "claude"
	task.Session.AgentHome = t.TempDir()
	task.Session.ConversationID = newConversationID()
	task.Session.Generation = 2
	dir := filepath.Join(task.Session.AgentHome, "projects", "test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(dir, task.Session.ConversationID+".jsonl")
	if err := os.WriteFile(history, []byte("old evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "pd-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	inbox := claudeInbox{PID: os.Getpid(), Conversation: task.Session.ConversationID, Generation: task.Session.Generation, Socket: filepath.Join(socketDir, "inbox")}
	listener, err := net.Listen("unix", inbox.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	frames := make(chan string, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(frames)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var frame struct {
				Type, From, Priority string
				FromMode             *string `json:"from_mode"`
				Message              struct{ Role, Content string }
			}
			err = json.NewDecoder(conn).Decode(&frame)
			conn.Close()
			if err != nil {
				continue // An offline peer check must send no frame.
			}
			if frame.Type != "user" || frame.From != "paddock" || frame.Priority != "next" || frame.FromMode != nil || frame.Message.Role != "user" {
				t.Error("message changed native admission policy", frame)
			}
			frames <- frame.Message.Content
			event := map[string]string{"type": "queue-operation", "operation": "enqueue", "sessionId": task.Session.ConversationID, "content": frame.Message.Content}
			switch admission {
			case "held", "refused":
				event["type"] = admission
			case "wrong-session":
				event["sessionId"] = "other"
			case "wrong-content":
				event["content"] = "another message"
			}
			data, _ := json.Marshal(event)
			log, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Error(err)
				return
			}
			// A native transcript record can arrive across multiple reads.
			log.Write(data[:len(data)/2])
			time.Sleep(10 * time.Millisecond)
			log.Write(append(data[len(data)/2:], '\n'))
			log.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return task, inbox, frames
}

func TestHostClaudeNativeAdmission(t *testing.T) {
	for _, admission := range []string{"queued", "held", "refused", "wrong-session", "wrong-content"} {
		t.Run(admission, func(t *testing.T) {
			task, inbox, frames := hostClaudeFixture(t, taskFixture(), admission)
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			id, err := deliverHostClaude(ctx, task, inbox, "fix the PR")
			if id != task.Session.ConversationID || (err == nil) != (admission == "queued") || errors.Is(err, ErrAgentOffline) {
				t.Fatalf("id=%q err=%v", id, err)
			}
			if got := <-frames; !strings.HasSuffix(got, "\nfix the PR") {
				t.Fatal("lost message", got)
			}
		})
	}
}

func TestHostClaudeOfflineSendsNoMessage(t *testing.T) {
	for _, failure := range []string{"missing-state", "generation", "conversation", "peer", "missing-history", "missing-socket"} {
		t.Run(failure, func(t *testing.T) {
			store := openTestStore(t, t.TempDir())
			task, inbox, frames := hostClaudeFixture(t, taskFixture(), "queued")
			if err := store.Insert(task); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "generation":
				inbox.Generation--
			case "conversation":
				inbox.Conversation = "other"
			case "peer":
				inbox.PID++
			case "missing-history":
				os.RemoveAll(filepath.Join(task.Session.AgentHome, "projects"))
			case "missing-socket":
				inbox.Socket += "-missing"
			}
			if failure != "missing-state" {
				if err := writeJSONAtomic(store.claudeInboxFile(task.ID), inbox); err != nil {
					t.Fatal(err)
				}
			}
			_, err := (Service{Store: store}).messageClaude(context.Background(), task, "hello")
			if !errors.Is(err, ErrAgentOffline) {
				t.Fatalf("expected safe offline error, got %v", err)
			}
			select {
			case frame := <-frames:
				t.Fatal("sent to offline or stale inbox", frame)
			default:
			}
		})
	}
}

func TestHostClaudeLaunchInbox(t *testing.T) {
	task := taskFixture()
	task.Session.Agent = "claude"
	task.Session.ConversationID = newConversationID()
	task.Session.AgentPath = script(t, "echo '"+claudeInboxVersion+"'")
	for _, resume := range []bool{false, true} {
		task.Session.Resume = resume
		command := AgentCommand(task, "/state", "/paddock")
		inbox, cleanup, err := prepareHostClaudeInbox(task, command)
		if err != nil {
			t.Fatal(err)
		}
		if command.Args[1] != "--messaging-socket-path" || command.Args[2] != inbox.Socket || inbox.Conversation != task.Session.ConversationID {
			t.Fatal("inbox launch lost identity", command.Args, inbox)
		}
		if info, err := os.Stat(filepath.Dir(inbox.Socket)); err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("inbox directory must be private", info, err)
		}
		cleanup()
		if _, err := os.Stat(filepath.Dir(inbox.Socket)); !os.IsNotExist(err) {
			t.Fatal("inbox directory leaked", err)
		}
	}
	task.Session.AgentPath = script(t, "echo 'unknown version'")
	if _, cleanup, err := prepareHostClaudeInbox(task, AgentCommand(task, "/state", "/paddock")); err == nil {
		cleanup()
		t.Fatal("accepted an unqualified inbox protocol")
	}
}

func TestHostClaudeReviewDeliveryAndDeduplication(t *testing.T) {
	g, remote, store, watch, _ := githubFixture(t)
	task, _ := store.Get(watch.TaskID)
	task, inbox, frames := hostClaudeFixture(t, task, "queued")
	if err := store.update(task.ID, func(current *Task) error { *current = task; return nil }); err != nil {
		t.Fatal(err)
	}
	summary, members := reviewFixture()
	remote.comments = append(members, summary)
	// Upgrading an already running host session must leave its review pending
	// until that same task has an inbox, rather than pretending it is online.
	if state := g.Service.WatchSessionState(context.Background(), task.ID); state != "offline" {
		t.Fatal("legacy session advertised delivery readiness", state)
	}
	if events, err := g.Poll(context.Background()); err != nil || len(events) != 0 || remote.reactionCalls != 0 {
		t.Fatal("consumed a review without an inbox", events, err)
	}
	if err := writeJSONAtomic(store.claudeInboxFile(task.ID), inbox); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := g.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	frame := <-frames
	for _, body := range []string{summary.Body, members[0].Body, members[1].Body} {
		if !strings.Contains(frame, body) {
			t.Fatal("review context was lost", body)
		}
	}
	if len(frames) != 0 || remote.reactionCalls != 2 {
		t.Fatal("duplicate delivery or incorrect receipts", len(frames), remote.reactionCalls)
	}
	state, _ := store.GitHubState()
	if state.Events[githubEventID(watch, summary)].Status != "done" {
		t.Fatal("review was not delivered")
	}
}

func TestHostClaudeUncertainReviewIsNotRetried(t *testing.T) {
	g, remote, store, watch, _ := githubFixture(t)
	task, _ := store.Get(watch.TaskID)
	task, inbox, frames := hostClaudeFixture(t, task, "held")
	if err := store.update(task.ID, func(current *Task) error { *current = task; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(store.claudeInboxFile(task.ID), inbox); err != nil {
		t.Fatal(err)
	}
	summary, members := reviewFixture()
	remote.comments = append(members, summary)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, _ = g.Poll(ctx)
	if _, err := g.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || remote.reactionCalls != 0 {
		t.Fatal("uncertain review was retried or acknowledged", len(frames), remote.reactionCalls)
	}
	state, _ := store.GitHubState()
	for _, comment := range append(members, summary) {
		if state.Events[githubEventID(watch, comment)].Status != "uncertain" {
			t.Fatal("review member lost uncertainty", comment.ID)
		}
	}
}

func TestHostClaudeSupervisorPublishesAndCleansInbox(t *testing.T) {
	exe := buildCLI(t)
	store := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Worktree = t.TempDir()
	task.Session.Agent = "claude"
	task.Session.ConversationID = newConversationID()
	task.Session.AgentPath = script(t, `if [ "$1" = --version ]; then echo '2.1.286 (Claude Code)'; exit 0; fi
printf '%s\n' "$$" "$@" > args
while [ ! -f finish ]; do sleep 0.05; done
`)
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	for generation := range 2 {
		if err := store.update(task.ID, func(current *Task) error {
			current.Status = "starting"
			current.Session.Generation = generation
			current.Session.Resume = generation > 0
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(task.Worktree, "finish"))
		command := exec.Command(exe, "__run", task.ID, "--generation", strconv.Itoa(generation))
		command.Env = append(os.Environ(), "PADDOCK_HOME="+store.Home)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		// Release the fixture on every test exit, including assertion failures.
		func() {
			defer func() { os.WriteFile(filepath.Join(task.Worktree, "finish"), nil, 0600); command.Wait() }()
			var inbox claudeInbox
			waitFor(t, func() bool {
				data, _ := os.ReadFile(store.claudeInboxFile(task.ID))
				return json.Unmarshal(data, &inbox) == nil && inbox.Generation == generation
			})
			waitFor(t, func() bool { _, err := os.Stat(filepath.Join(task.Worktree, "args")); return err == nil })
			data, err := os.ReadFile(filepath.Join(task.Worktree, "args"))
			if err != nil || !strings.HasPrefix(string(data), strconv.Itoa(inbox.PID)+"\n--messaging-socket-path\n"+inbox.Socket+"\n") || inbox.Conversation != task.Session.ConversationID {
				t.Fatalf("inbox is not bound to native process: %+v args=%s err=%v", inbox, data, err)
			}
			if generation > 0 && !strings.Contains(string(data), "\n--resume\n"+task.Session.ConversationID+"\n") {
				t.Fatal("resume lost conversation identity", string(data))
			}
			os.Remove(filepath.Join(task.Worktree, "args"))
		}()
		if _, err := os.Stat(store.claudeInboxFile(task.ID)); !os.IsNotExist(err) {
			t.Fatal("supervisor leaked inbox state", err)
		}
	}
}
