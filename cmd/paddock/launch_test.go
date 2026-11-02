package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestLaunchDisplay(t *testing.T) {
	for _, tc := range []struct {
		name, command, backend, taskEnv string
		flags                           map[string]string
		wantAttach, reject              bool
	}{
		{name: "run opens Rex", command: "run", backend: "rex", wantAttach: true},
		{name: "explicit client", command: "run", backend: "rex", flags: map[string]string{"client": "client:chosen"}, wantAttach: true},
		{name: "remote control rejected", command: "run", backend: "rex", wantAttach: true, reject: true},
		{name: "detached", command: "run", backend: "rex", flags: map[string]string{"detach": "true"}},
		{name: "JSON stays machine readable", command: "run", backend: "rex", flags: map[string]string{"json": "true"}},
		{name: "delegate stays background", command: "delegate", backend: "rex"},
		{name: "agent run stays background", command: "run", backend: "rex", taskEnv: "parent"},
		{name: "Screen stays detached", command: "run", backend: "screen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PADDOCK_TASK_ID", tc.taskEnv)
			task := paddock.Task{ID: "created-task", Session: paddock.Session{Backend: tc.backend, ID: "session:created"}}
			var out, warnings bytes.Buffer
			called := false
			err := presentLaunchedTask(context.Background(), tc.command, task, tc.flags, &out, &warnings, func(_ context.Context, got paddock.Task, client string) error {
				called = true
				if got.ID != task.ID || got.Session.ID != task.Session.ID || client != tc.flags["client"] {
					t.Fatal("selected wrong task or client")
				}
				if tc.reject {
					return errors.New("remote control disabled")
				}
				return nil
			})
			if err != nil || called != tc.wantAttach {
				t.Fatalf("attach=%v err=%v", called, err)
			}
			if tc.reject && (!strings.Contains(warnings.String(), "remote control disabled") || !strings.Contains(warnings.String(), "paddock attach created-task")) {
				t.Fatal("missing recoverable display diagnostic")
			}
			if !tc.reject && warnings.Len() != 0 {
				t.Fatal(warnings.String())
			}
			if tc.flags["json"] != "" {
				var got paddock.Task
				if json.Unmarshal(out.Bytes(), &got) != nil || got.ID != task.ID {
					t.Fatal("corrupted JSON")
				}
			}
		})
	}
}
