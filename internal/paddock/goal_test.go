package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGoalPromptParsing(t *testing.T) {
	for _, tc := range []struct {
		prompt, objective string
		goal, invalid     bool
	}{
		{"ordinary task", "", false, false},
		{"explain /goal usage", "", false, false},
		{"/goalkeeper", "", false, false},
		{" /goal Build it\nwith tests ", "Build it\nwith tests", true, false},
		{"/goal\u2003Build it", "Build it", true, false},
		{"/goal", "", true, true},
		{"/goal \n\t", "", true, true},
		{"/goal pause", "", true, true},
		{"/goal resume", "", true, true},
		{"/goal clear", "", true, true},
		{"/goal pause the queue", "pause the queue", true, false},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			objective, goal, err := parseGoalPrompt(tc.prompt)
			if objective != tc.objective || goal != tc.goal || (err != nil) != tc.invalid {
				t.Fatal(objective, goal, err)
			}
		})
	}
}

func TestNativeGoalObjectiveLimit(t *testing.T) {
	for _, size := range []int{3999, 4000, 4001, 6000} {
		full := strings.Repeat("😀", size)
		got := nativeGoalObjective(full)
		if utf8.RuneCountInString(got) > 4000 || !utf8.ValidString(got) {
			t.Fatal("invalid or oversized native objective")
		}
		if size <= 4000 && got != full {
			t.Fatal("changed an objective within the native limit")
		}
		if size > 4000 && !strings.Contains(got, "full goal specification") {
			t.Fatal("lost the full specification reference")
		}
	}
	if got := nativeGoalObjective("# User's goal title\n\n" + strings.Repeat("x", 5000)); !strings.HasPrefix(got, "# User's goal title\n\n") {
		t.Fatal(got)
	}
}

func TestGoalStartupRetryRequiresProofOfUnstartedTask(t *testing.T) {
	task := taskFixture()
	task.Prompt, task.Status, task.Error = "/goal Build it", "failed", "Codex rejected thread/goal/set; use a Codex build with Goals enabled"
	code := 1
	task.Session = Session{Agent: "codex", Generation: 1, ExitCode: &code}
	if !canRetryInitialGoalStartup(task) {
		t.Fatal("cannot recover the original startup failure")
	}
	task.Error = "Codex goal setup failed: unavailable"
	if !canRetryInitialGoalStartup(task) {
		t.Fatal("cannot recover a confirmed setup failure")
	}
	for _, change := range []func(*Task){
		func(t *Task) { t.Error = "unrelated failure" },
		func(t *Task) { t.Session.Resume = true },
		func(t *Task) { t.Session.GoalInitialized = true },
		func(t *Task) { t.Session.ConversationID = "saved-thread" },
		func(t *Task) { t.Session.ExitCode = nil },
		func(t *Task) { t.Session.Agent = "claude" },
	} {
		changed := task
		change(&changed)
		if canRetryInitialGoalStartup(changed) {
			t.Fatal("allowed replay without proof of an unstarted task", changed)
		}
	}
}

func TestGoalPreparationAndResume(t *testing.T) {
	for _, failure := range []string{"", "thread/start", "thread/goal/set", "thread/inject_items", "thread/goal/get"} {
		t.Run("failure="+failure, func(t *testing.T) {
			store := openTestStore(t, t.TempDir())
			task := taskFixture()
			task.Prompt, task.Runtime, task.Worktree = "/goal Build it\nwith tests", "host", t.TempDir()
			task.launchNotice = "Fixture availability notice"
			log := filepath.Join(t.TempDir(), "requests.jsonl")
			agent := filepath.Join(t.TempDir(), "codex")
			const thread = "00000000-0000-4000-8000-000000000001"
			fixture := fmt.Sprintf(`#!/usr/bin/env python3
import json, sys
for line in sys.stdin:
    request = json.loads(line)
    with open(%q, 'a') as log:
        log.write(json.dumps(request) + '\n')
    if 'id' not in request:
        continue
    method = request['method']
    params = request['params']
    result = {}
    if method == %q:
        print(json.dumps({'id': request['id'], 'error': {'code': -1, 'message': 'fixture failure'}}), flush=True)
        continue
    if method in ('thread/start', 'thread/read'):
        result = {'thread': {'id': %q, 'cwd': %q}}
    if method in ('thread/goal/set', 'thread/goal/get'):
        result = {'goal': {'threadId': %q, 'objective': 'Build it\nwith tests', 'status': params.get('status', 'paused')}}
    print(json.dumps({'id': request['id'], 'result': result}), flush=True)
`, log, failure, thread, task.Worktree, thread)
			if err := os.WriteFile(agent, []byte(fixture), 0755); err != nil {
				t.Fatal(err)
			}
			task.Session = Session{Agent: "codex", AgentPath: agent}
			if err := store.Insert(task); err != nil {
				t.Fatal(err)
			}
			err := prepareInitialGoal(context.Background(), store, &task, "/fixture/paddock")
			if (err != nil) != (failure != "") {
				t.Fatal(err)
			}
			saved, getErr := store.Get(task.ID)
			if getErr != nil || saved.Session.GoalInitialized != (failure == "") {
				t.Fatal(saved.Session, getErr)
			}
			if failure != "" {
				return
			}
			if saved.Session.ConversationID != thread {
				t.Fatal(saved.Session)
			}
			command := AgentCommand(saved, store.Home, "/fixture/paddock")
			if !reflect.DeepEqual(command.Args[1:], []string{"resume", thread}) {
				t.Fatal("goal replayed as ordinary prompt", command.Args)
			}
			before, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if err = prepareInitialGoal(context.Background(), store, &saved, "/fixture/paddock"); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(log)
			if string(after) != string(before) {
				t.Fatal("resume re-created the goal")
			}
			var methods []string
			for _, line := range strings.Split(strings.TrimSpace(string(before)), "\n") {
				var request struct {
					Method string
					Params map[string]any
				}
				if err := json.Unmarshal([]byte(line), &request); err != nil {
					t.Fatal(err)
				}
				methods = append(methods, request.Method)
				if request.Method == "thread/start" && (request.Params["sandbox"] != nil || request.Params["approvalPolicy"] != nil) {
					t.Fatal("overrode host permissions", request.Params)
				}
				if request.Method == "thread/goal/set" && request.Params["origin"] != "user" {
					t.Fatal("lost user authorization", request.Params)
				}
				if request.Method == "thread/goal/set" && request.Params["tokenBudget"] != nil {
					t.Fatal("invented a goal budget", request.Params)
				}
				if request.Method == "thread/inject_items" && (!strings.Contains(line, "Paddock context") || !strings.Contains(line, "Fixture availability notice")) {
					t.Fatal("lost startup context", line)
				}
			}
			want := []string{"initialize", "initialized", "thread/start", "thread/goal/set", "thread/inject_items", "initialize", "initialized", "thread/read", "thread/goal/get", "thread/goal/set"}
			if !slices.Equal(methods, want) {
				t.Fatal(methods)
			}
			// Recover the saved paused-thread checkpoint after an interrupted
			// activation, preserving its native identity and original context.
			saved.Session.GoalInitialized = false
			if err := store.UpdateSession(saved.ID, func(v *Session) { v.GoalInitialized = false }); err != nil {
				t.Fatal(err)
			}
			if err := prepareInitialGoal(context.Background(), store, &saved, "/fixture/paddock"); err != nil {
				t.Fatal(err)
			}
			after, _ = os.ReadFile(log)
			if strings.Contains(string(after[len(before):]), `"method": "thread/start"`) || strings.Contains(string(after[len(before):]), `"method": "thread/inject_items"`) {
				t.Fatal("recovery created a replacement conversation or replayed context")
			}
			if saved.Session.ConversationID != thread || !saved.Session.GoalInitialized {
				t.Fatal(saved.Session)
			}
		})
	}
}

func TestNewGoalRejectsClaudeBeforeCreatingTask(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	_, err := (Service{Store: store}).Run(context.Background(), RunOptions{Repo: repoFixture(t), Prompt: "/goal Build it", Runtime: "host", Agent: "claude"})
	if err == nil || !strings.Contains(err.Error(), "/goal requires the Codex agent") {
		t.Fatal(err)
	}
	tasks, err := store.List("")
	if err != nil || len(tasks) != 0 {
		t.Fatal(tasks, err)
	}
}

func TestGoalMetadataUsesSandboxAndInvestigationGuard(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(readOnly), func(t *testing.T) {
			task := taskFixture()
			task.Runtime = "openshell"
			task.Launcher = &LauncherConfig{ReadOnly: readOnly}
			task.OpenShell = &OpenShellTask{Name: "fixture", Config: OpenShellConfig{Binary: script(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
  esac
done
`)}}
			task.Session.Agent, task.Session.AgentPath = "codex", "/usr/local/bin/codex"
			rpc, err := openGoalRPC(context.Background(), task, "/fixture/state", "/fixture/paddock")
			if err != nil {
				t.Fatal(err)
			}
			defer rpc.close()
			args := rpc.command.Args
			start := slices.Index(args, "--")
			if start == -1 || !slices.Contains(args[:start], "--no-tty") {
				t.Fatal("metadata escaped sandbox transport", args)
			}
			want := []string{"/usr/local/bin/codex", "app-server", "--stdio"}
			if readOnly {
				want = append([]string{"/usr/bin/python3", investigationGuardDir + "/exec.py"}, want...)
			}
			if !slices.Equal(args[start+1:], want) {
				t.Fatal("metadata bypassed the agent's guard", args)
			}
		})
	}
}
