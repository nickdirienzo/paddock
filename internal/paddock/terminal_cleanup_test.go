package paddock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveRexTerminalCleanup(t *testing.T) {
	if os.Getenv("PADDOCK_LIVE_TERMINAL") != "rex" {
		t.Skip("set PADDOCK_LIVE_TERMINAL=rex to exercise real session cleanup")
	}
	for _, shared := range []bool{false, true} {
		name := "empty session"
		if shared {
			name = "shared session"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, session, err := SelectTerminal(ctx, "rex")
			if err != nil {
				t.Fatal(err)
			}
			term := terminal{backend: "rex", path: session.TerminalPath, endpoint: session.Endpoint}
			b, err := term.rex(ctx, "new", "paddock-cleanup-test", "--shell=none", "--json", "--", "/bin/sh", "-c", "exit 7")
			if err != nil {
				t.Fatal(err)
			}
			var created struct {
				ID      string `json:"session_id"`
				Windows []struct {
					ID     string   `json:"window_id"`
					Blocks []string `json:"block_ids"`
				} `json:"initial_windows"`
			}
			if err := json.Unmarshal(b, &created); err != nil {
				t.Fatal(err)
			}
			if created.ID == "" {
				t.Fatal("missing session ID", string(b))
			}
			task := Task{Session: session}
			task.Session.ID = created.ID
			t.Cleanup(func() { cleanupTerminal(task) })
			if len(created.Windows) == 0 || len(created.Windows[0].Blocks) == 0 {
				t.Fatal("missing owned pane", string(b))
			}
			task.Session.WindowID, task.Session.BlockID = created.Windows[0].ID, created.Windows[0].Blocks[0]
			if shared {
				if _, err := term.rex(ctx, "window", "new", "-s", created.ID, "User pane", "--focus=false", "--shell=none", "--json", "--", "/bin/sleep", "60"); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, func() bool {
				_, obs := term.Inspect(ctx, task)
				return obs.State == "exited" || obs.State == "missing"
			})
			for attempt := 0; attempt < 2; attempt++ {
				if err := closeExitedTerminal(ctx, task); err != nil {
					t.Fatal(err)
				}
				b, err := term.rex(ctx, "ls", "--json")
				if err != nil {
					t.Fatal(err)
				}
				var listed struct {
					Sessions []struct {
						ID string `json:"session_id"`
					} `json:"sessions"`
				}
				if err := json.Unmarshal(b, &listed); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, session := range listed.Sessions {
					found = found || session.ID == created.ID
				}
				if found != shared {
					t.Fatalf("session retained = %t, want %t", found, shared)
				}
			}
		})
	}
}

func TestRexTerminalCleanup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		blocks     string
		process    string
		missing    bool
		listFails  bool
		closeFails bool
		killFails  bool
		wantError  bool
		wantKill   bool
		retryError bool
	}{
		{name: "last pane removes header", blocks: `{"blocks":[]}`, wantKill: true},
		{name: "already removed pane removes header", missing: true, blocks: `{"blocks":[]}`, wantKill: true},
		{name: "sibling remains", blocks: `{"blocks":[{"block_id":"block:sibling"}]}`},
		{name: "user pane appeared during close", blocks: `{"blocks":[{"block_id":"block:user"}]}`},
		{name: "detached block remains", blocks: `{"blocks":[{"block_id":"block:detached","placement":"detached"}]}`},
		{name: "missing pane with sibling", missing: true, blocks: `{"blocks":[{"block_id":"block:sibling"}]}`},
		{name: "unknown block retained", blocks: `{"blocks":[{}]}`, retryError: true},
		{name: "missing block list", blocks: `{}`, wantError: true},
		{name: "null block list", blocks: `{"blocks":null}`, wantError: true},
		{name: "invalid block list", blocks: `invalid`, wantError: true},
		{name: "block list unavailable", listFails: true, wantError: true},
		{name: "close failure", closeFails: true, wantError: true},
		{name: "kill response lost", blocks: `{"blocks":[]}`, killFails: true, wantError: true, wantKill: true},
		{name: "running process", process: `{"child":{"pid":42}}`, wantError: true},
		{name: "unknown process", process: `{}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := taskFixture()
			task.Session.Backend, task.Session.ID, task.Session.BlockID = "rex", "session:task", "block:owned"
			task.Session.Generation = 1
			task.Idle = &IdleState{Generation: 1, DrainConfirmed: true}
			dir := t.TempDir()
			log, closed, gone := filepath.Join(dir, "calls"), filepath.Join(dir, "closed"), filepath.Join(dir, "gone")
			if tc.missing {
				if err := os.WriteFile(closed, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			process := tc.process
			if process == "" {
				process = `{"child":null,"last_exit":{"exit_code":0}}`
			}
			listing := "printf '%s' " + quote(tc.blocks)
			if tc.listFails {
				listing = "exit 1"
			}
			closeCommand, killCommand := "touch "+quote(closed), "touch "+quote(gone)
			if tc.closeFails {
				closeCommand = "exit 1"
			}
			if tc.killFails {
				killCommand += "; exit 1" // Deletion succeeded, but the response was lost.
			}
			task.Session.TerminalPath = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'block ls '*) `+listing+`;;
 *'ls --json')
   if test -f `+quote(gone)+`; then printf '%s' '{"sessions":[]}';
   else printf '%s' '{"sessions":[{"session_id":"session:task"}]}'; fi;;
 *'process')
   if test -f `+quote(closed)+`; then exit 1; fi
   printf '%s' `+quote(process)+`;;
 *'block close -s session:task block:owned') `+closeCommand+`;;
 *'kill session:task') `+killCommand+`;;
 *) exit 1;;
esac`)
			err := closeExitedTerminal(context.Background(), task)
			if (err != nil) != tc.wantError {
				t.Fatalf("cleanup error = %v, wantError = %t", err, tc.wantError)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(calls), "kill session:task"); got != tc.wantKill {
				t.Fatalf("killed = %t, want %t: %s", got, tc.wantKill, calls)
			}
			if !tc.missing && tc.process == "" && !strings.Contains(string(calls), "block close -s session:task block:owned") {
				t.Fatal("owned pane was not closed", string(calls))
			}
			if tc.process != "" && strings.Contains(string(calls), "block close") {
				t.Fatal("closed an active or unknown process", string(calls))
			}
			if tc.wantKill && strings.Index(string(calls), "block ls") > strings.Index(string(calls), "kill session:task") {
				t.Fatal("deleted session without checking remaining blocks", string(calls))
			}
			if !tc.wantError || tc.killFails {
				if err := closeExitedTerminal(context.Background(), task); (err != nil) != tc.retryError {
					t.Fatalf("cleanup retry error = %v, wantError = %t", err, tc.retryError)
				}
				again, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(again), "kill session:task") != strings.Count(string(calls), "kill session:task") {
					t.Fatal("cleanup repeated session deletion", string(again))
				}
			}
		})
	}
}
