package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sandboxMessageFixture(t *testing.T) (Service, Task, Task, sandboxMessageRequest) {
	t.Helper()
	store := openTestStore(t, t.TempDir())
	parent, source := taskFixture(), taskFixture()
	source.Runtime, source.ParentID = "openshell", parent.ID
	source.OpenShell = &OpenShellTask{Ready: true}
	for _, task := range []Task{parent, source} {
		if err := store.Insert(task); err != nil {
			t.Fatal(err)
		}
	}
	return Service{Store: store}, source, parent, sandboxMessageRequest{ID: newID(), Target: parent.ID, Body: "Findings: ~2s in DuckDB.\nLiteral $(echo unsafe) `whoami`", ExternalID: "report-1"}
}

func TestSandboxMessageAuthorizationAndLimits(t *testing.T) {
	for _, variant := range []string{"unrelated target", "spoofed parent", "no parent", "host source", "not ready", "empty body", "large body", "bad request ID", "empty key", "large key"} {
		t.Run(variant, func(t *testing.T) {
			s, source, parent, request := sandboxMessageFixture(t)
			switch variant {
			case "unrelated target":
				request.Target = newID()
			case "spoofed parent":
				request.Target, source.ParentID = newID(), newID()
			case "no parent":
				if err := s.Store.update(source.ID, func(task *Task) error { task.ParentID = ""; return nil }); err != nil {
					t.Fatal(err)
				}
			case "host source":
				if err := s.Store.update(source.ID, func(task *Task) error { task.Runtime = "host"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "not ready":
				if err := s.Store.update(source.ID, func(task *Task) error { task.OpenShell.Ready = false; return nil }); err != nil {
					t.Fatal(err)
				}
			case "empty body":
				request.Body = " \n"
			case "large body":
				request.Body = strings.Repeat("é", sandboxMessageLimit/2+1)
			case "bad request ID":
				request.ID = "../../../host"
			case "empty key":
				request.ExternalID = ""
			case "large key":
				request.ExternalID = strings.Repeat("a", 257)
			}
			result := s.deliverSandboxMessage(context.Background(), source, request, func(context.Context, Task, string) (string, error) {
				t.Fatal("unauthorized message delivered to", parent.ID)
				return "", nil
			})
			if result.Status != "rejected" || result.Error == "" {
				t.Fatalf("accepted invalid request: %+v", result)
			}
		})
	}
}

func TestSandboxMessageReceiptsAndRetry(t *testing.T) {
	for _, variant := range []string{"delivered", "offline then delivered", "uncertain", "crashed after claim", "parent busy"} {
		t.Run(variant, func(t *testing.T) {
			s, source, parent, request := sandboxMessageFixture(t)
			body := "Paddock message from child task " + source.ID + ":\n\n" + request.Body
			calls := 0
			deliver := func(_ context.Context, task Task, message string) (string, error) {
				calls++
				if task.ID != parent.ID || message != body {
					t.Fatal("lost sender or content", task.ID, message)
				}
				if variant == "offline then delivered" && calls == 1 {
					return "", ErrAgentOffline
				}
				if variant == "uncertain" {
					return "thread", errors.New("queue acknowledgement lost")
				}
				return "thread", nil
			}
			if variant == "crashed after claim" {
				if _, _, err := s.Store.ClaimMessage(parent.ID, "sandbox:"+source.ID, request.ExternalID, body); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "parent busy" {
				release, err := s.Store.taskLease(parent.ID, "operation")
				if err != nil {
					t.Fatal(err)
				}
				result := s.deliverSandboxMessage(context.Background(), source, request, deliver)
				release()
				if result.Status != "pending" || calls != 0 {
					t.Fatal("busy parent treated as sent", result, calls)
				}
			}
			first := s.deliverSandboxMessage(context.Background(), source, request, deliver)
			if variant == "offline then delivered" {
				if first.Status != "pending" || s.noPendingMessages(context.Background(), source) == nil {
					t.Fatal("lost offline message or source retention", first)
				}
			}
			result := s.deliverSandboxMessage(context.Background(), source, request, deliver)
			want, count := "delivered", 1
			switch variant {
			case "offline then delivered":
				count = 2
			case "uncertain":
				want = "uncertain"
			case "crashed after claim":
				want, count = "uncertain", 0
			}
			if result.Status != want || calls != count {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
			request.Body = "changed content"
			if changed := s.deliverSandboxMessage(context.Background(), source, request, deliver); changed.Status != "rejected" || calls != count {
				t.Fatal("reused key accepted changed content", changed, calls)
			}
		})
	}
}

func TestSandboxParentMessageClientAndNativeDelivery(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required")
	}
	g, _, store, watch, nativeLog := githubFixture(t)
	parent, err := store.Get(watch.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	client := filepath.Join(root, "paddock")
	if err = os.WriteFile(client, []byte(strings.ReplaceAll(sandboxShellClient, "/sandbox/.paddock", root)), 0700); err != nil {
		t.Fatal(err)
	}
	source := taskFixture()
	source.Runtime, source.ParentID, source.Status = "openshell", parent.ID, "running"
	// Emulate the OpenShell exec boundary, retaining argv exactly. The actual
	// Python request reader and acknowledgement scripts run against a fixture.
	bridge := filepath.Join(t.TempDir(), "openshell")
	bridgeCode := fmt.Sprintf(`#!%s
import os,sys
args=sys.argv[sys.argv.index('--')+1:]
args=[arg.replace('/sandbox/.paddock',%q) for arg in args]
os.execvp(args[0],args)
`, python, root)
	if err = os.WriteFile(bridge, []byte(bridgeCode), 0700); err != nil {
		t.Fatal(err)
	}
	source.OpenShell = &OpenShellTask{Name: "owned", Ready: true, Config: OpenShellConfig{Binary: bridge}}
	if err = store.Insert(source); err != nil {
		t.Fatal(err)
	}
	body := "Full report\nUnicode ✓ and literal $(touch BAD) `whoami`"
	runClient := func(body string) (*exec.Cmd, string) {
		t.Helper()
		output := filepath.Join(t.TempDir(), "output")
		file, err := os.Create(output)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		cmd := exec.Command(python, client, "message", parent.ID, body, "--external-id", "investigation-report", "--json")
		cmd.Stdout, cmd.Stderr = file, file
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, output
	}
	cmd, output := runClient(body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(filepath.Join(root, "message-requests"))
		if len(entries) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = g.Service.noPendingMessages(context.Background(), source); err == nil {
		t.Fatal("child could expire with an unpolled parent report")
	}
	if err = g.Service.pollSandboxMessage(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		data, _ := os.ReadFile(output)
		t.Fatalf("%v: %s", err, data)
	}
	data, _ := os.ReadFile(output)
	var result sandboxMessageResult
	if json.Unmarshal(data, &result) != nil || result.Status != "delivered" || result.ThreadID != "thread-fixture" {
		t.Fatalf("missing native acknowledgement: %s", data)
	}
	if err = g.Service.noPendingMessages(context.Background(), source); err != nil {
		t.Fatal("delivered report still blocks child expiry", err)
	}
	// A repeated command with the same key returns a receipt without enqueueing
	// twice, including a lost sandbox acknowledgement after native delivery.
	if err = os.Remove(filepath.Join(root, "message-response-"+result.ID)); err != nil {
		t.Fatal(err)
	}
	cmd, output = runClient(body)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err = g.Service.pollSandboxMessage(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(root, "message-response-"+result.ID)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(nativeLog)
	if strings.Count(string(data), "queue --thread") != 1 || !strings.Contains(string(data), body) || !strings.Contains(string(data), source.ID) {
		t.Fatalf("missing, modified, or duplicate native delivery: %s", data)
	}
	cmd, _ = runClient("different content")
	if err = cmd.Wait(); err == nil {
		t.Fatal("client reported stale delivery for changed body")
	}
	if err = g.Service.pollSandboxMessage(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	// Malformed files and symlinks cannot prevent the reader from finding a
	// subsequent well-formed request or cause host file reads.
	requests := filepath.Join(root, "message-requests")
	if err = os.WriteFile(filepath.Join(requests, strings.Repeat("0", 24)), []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(output, filepath.Join(requests, strings.Repeat("1", 24))); err != nil {
		t.Fatal(err)
	}
	request := sandboxMessageRequest{ID: strings.Repeat("f", 24), Target: parent.ID, Body: strings.Repeat("\\", sandboxMessageLimit), ExternalID: "second-report"}
	if err = writeJSONAtomic(filepath.Join(requests, request.ID), request); err != nil {
		t.Fatal(err)
	}
	if err = g.Service.pollSandboxMessage(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "message-response-"+request.ID))
	if json.Unmarshal(data, &result) != nil || result.Status != "delivered" {
		t.Fatalf("reader blocked by invalid files: %s", data)
	}
}
