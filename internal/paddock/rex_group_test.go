package paddock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRexFamilyDestination(t *testing.T) {
	for _, tc := range []struct {
		name, parentID, parentGroup, ownGroup, listing, want string
		otherEndpoint, unreachable                           bool
	}{
		{name: "parent", parentID: "session:family", listing: `{"sessions":[{"session_id":"session:family"}]}`, want: "session:family"},
		{name: "parent metadata pending", listing: `{"sessions":[{"session_id":"session:family","label":"parent-name"}]}`, want: "session:family"},
		{name: "nested metadata pending", parentGroup: "session:family", listing: `{"sessions":[{"session_id":"session:family"}]}`, want: "session:family"},
		{name: "closed section", parentID: "session:gone", listing: `{"sessions":[]}`},
		{name: "resume joins parent", parentID: "session:family", ownGroup: "session:old", listing: `{"sessions":[{"session_id":"session:old"},{"session_id":"session:family"}]}`, want: "session:family"},
		{name: "resume retained section", parentID: "session:gone", ownGroup: "session:old", listing: `{"sessions":[{"session_id":"session:old"}]}`, want: "session:old"},
		{name: "other server", parentID: "session:family", otherEndpoint: true},
		{name: "unreachable", parentID: "session:family", unreachable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t, t.TempDir())
			parent := taskFixture()
			parent.Session = Session{Backend: "rex", ID: tc.parentID, GroupID: tc.parentGroup, Name: "parent-name", Endpoint: "unix:///rex"}
			if tc.otherEndpoint {
				parent.Session.Endpoint = "unix:///other"
			}
			if err := store.Insert(parent); err != nil {
				t.Fatal(err)
			}
			child := taskFixture()
			child.ID, child.ParentID = newID(), parent.ID
			child.Session = Session{Backend: "rex", Endpoint: "unix:///rex", GroupID: tc.ownGroup}
			body := "printf '%s' " + quote(tc.listing)
			if tc.unreachable {
				body = "exit 1"
			}
			child.Session.TerminalPath = script(t, body)
			if err := store.Insert(child); err != nil {
				t.Fatal(err)
			}
			err := (Service{Store: store}).prepareRexGroup(context.Background(), &child)
			if tc.unreachable {
				if err == nil {
					t.Fatal("unreachable server must not launch elsewhere")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := store.Get(child.ID)
			if err != nil || child.Session.GroupID != tc.want || saved.Session.GroupID != tc.want {
				t.Fatalf("memory=%q saved=%q want=%q err=%v", child.Session.GroupID, saved.Session.GroupID, tc.want, err)
			}
		})
	}
}

func TestRexGroupedLaunchAndLostReply(t *testing.T) {
	for _, runtime := range []string{"host", "openshell"} {
		t.Run(runtime, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls")
			task := taskFixture()
			task.Runtime = runtime
			task.Session = Session{Backend: "rex", GroupID: "session:family", Name: "child-name", Generation: 1}
			task.Session.TerminalPath = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'window new '*) printf '%s' '{"window_id":"window:child","block_ids":["block:child"]}' ;;
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:family"}]}' ;;
 *'session inspect '*) printf '%s' '{"windows":[{"window_id":"window:parent","label":"parent","focused_block_id":"block:parent"},{"window_id":"window:child","label":"child-name","focused_block_id":"block:child"}]}' ;;
 *'process') printf '%s' '{"child":{"pid":42}}' ;;
 *) exit 1 ;;
esac`)
			term := terminalFor(task.Session)
			launched, err := term.Launch(context.Background(), task, "/tmp/paddock", "/tmp/state")
			if err != nil {
				t.Fatal(err)
			}
			if launched.ID != "session:family" || launched.WindowID != "window:child" || launched.BlockID != "block:child" {
				t.Fatal(launched)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), "window new -s session:family child-name --focus=false") {
				t.Fatal(string(calls))
			}
			recovered, obs := term.Inspect(context.Background(), task)
			if obs.State != "running" || recovered.ID != launched.ID || recovered.WindowID != launched.WindowID || recovered.BlockID != launched.BlockID {
				t.Fatalf("recovered=%+v observation=%+v", recovered, obs)
			}
		})
	}
}

func TestRexGroupedRecoveryDoesNotAdoptSibling(t *testing.T) {
	task := taskFixture()
	task.Session = Session{Backend: "rex", GroupID: "session:family", Name: "missing-child"}
	task.Session.TerminalPath = script(t, `case "$*" in
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:family"}]}' ;;
 *'session inspect '*) printf '%s' '{"windows":[{"window_id":"window:parent","label":"parent","focused_block_id":"block:parent"}]}' ;;
 *) exit 1 ;;
esac`)
	recovered, obs := terminalFor(task.Session).Inspect(context.Background(), task)
	if obs.State != "missing" || recovered.BlockID != "" {
		t.Fatalf("%+v %+v", recovered, obs)
	}
}

func TestRexClosedTaskWindowWithLiveSiblings(t *testing.T) {
	task := taskFixture()
	task.Session = Session{Backend: "rex", ID: "session:family", WindowID: "window:child", BlockID: "block:child"}
	task.Session.TerminalPath = script(t, `case "$*" in
 *'block ls '*) printf '%s' '{"blocks":[{"block_id":"block:parent"}]}' ;;
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:family"}]}' ;;
 *'session inspect '*) printf '%s' '{"windows":[{"window_id":"window:parent"}]}' ;;
 *) exit 1 ;;
esac`)
	_, obs := terminalFor(task.Session).Inspect(context.Background(), task)
	if obs.State != "missing" {
		t.Fatal(obs)
	}
}
