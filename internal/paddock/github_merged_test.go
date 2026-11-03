package paddock

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMergedPRLabelsOfflineRexSession(t *testing.T) {
	g, remote, store, watch, _ := githubFixture(t)
	log := filepath.Join(t.TempDir(), "calls")
	renamed := filepath.Join(t.TempDir(), "renamed")
	path := script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'ls --json')
  if test -f `+quote(renamed)+`; then
   printf '%s' '{"sessions":[{"session_id":"session:owned","label":"threat-report-status ✓ merged"}]}'
  else
   printf '%s' '{"sessions":[{"session_id":"session:other","label":"keep me"},{"session_id":"session:owned","label":"threat-report-status"}]}'
  fi ;;
 *'api call session.set_label '*) touch `+quote(renamed)+` ;;
 *) exit 1 ;;
esac
`)
	if err := store.update(watch.TaskID, func(task *Task) error {
		task.Status = "stopped"
		task.Session.ConversationID = ""
		task.Session.Backend = "rex"
		task.Session.ID = "session:owned"
		task.Session.TerminalPath = path
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(watch.TaskID)
	remote.pr.State, remote.pr.Merged = "closed", true
	remote.comments = []GitHubComment{testComment(1, "do more work", 7)}
	for range 2 {
		state, err := store.GitHubState()
		if err != nil {
			t.Fatal(err)
		}
		events, err := g.pollWatch(context.Background(), state, watch.Key())
		if err != nil || len(events) != 0 {
			t.Fatalf("events=%v err=%v", events, err)
		}
	}
	after, _ := store.Get(watch.TaskID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("merge display changed task state or workspace activity")
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "api call session.set_label") != 1 ||
		!strings.Contains(string(calls), `{"label":"threat-report-status ✓ merged","session_id":"session:owned"}`) {
		t.Fatalf("expected one rename of the existing label: %s", calls)
	}
}

func TestMergeLabelRequiresMergedOwnedPR(t *testing.T) {
	for _, tc := range []struct {
		name, state          string
		merged, changedOwner bool
	}{
		{"closed without merge", "closed", false, false},
		{"open", "open", false, false},
		{"different owner", "closed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, remote, store, watch, _ := githubFixture(t)
			if err := store.update(watch.TaskID, func(task *Task) error {
				task.Status = "stopped"
				task.Session.Backend = "rex"
				task.Session.ID = "session:owned"
				task.Session.TerminalPath = script(t, "echo 'unexpected Rex call' >&2; exit 1")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			remote.pr.State, remote.pr.Merged = tc.state, tc.merged
			if tc.changedOwner {
				remote.pr.Head.Ref = "other-branch"
			}
			state, _ := store.GitHubState()
			_, err := g.pollWatch(context.Background(), state, watch.Key())
			if tc.changedOwner {
				if err == nil || !strings.Contains(err.Error(), "ownership changed") {
					t.Fatalf("expected ownership rejection: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMergeLabelMissingSessionAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"missing", `echo '{"sessions":[]}'`, false},
		{"already marked", `echo '{"sessions":[{"session_id":"session:owned","label":"task ✓ merged"}]}'`, false},
		{"unreachable", `exit 1`, true},
		{"invalid response", `echo 'not json'`, true},
		{"rename rejected", `case "$*" in *'ls --json') echo '{"sessions":[{"session_id":"session:owned","label":"task"}]}' ;; *) exit 1 ;; esac`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := markRexSessionMerged(context.Background(), Session{Backend: "rex", ID: "session:owned", TerminalPath: script(t, tc.body)})
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if err := markRexSessionMerged(context.Background(), Session{Backend: "screen"}); err != nil {
		t.Fatal(err)
	}
}
