package paddock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectionFailureDoesNotInterruptTask(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	v := taskFixture()
	v.Status = "running"
	v.Session.TerminalPath = script(t, "echo 'server unreachable' >&2; exit 1\n")
	if e := s.Insert(v); e != nil {
		t.Fatal(e)
	}
	got, obs, e := (Service{Store: s}).Refresh(context.Background(), v)
	if e != nil || obs.State != "unknown" || got.Status != "running" {
		t.Fatalf("%+v %+v %v", got, obs, e)
	}
}
func TestConfirmedMissingAndCompletedStates(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	v := taskFixture()
	v.Status = "running"
	v.Session.TerminalPath = script(t, "echo 'No Sockets found in /tmp/screen.'; exit 1\n")
	if e := s.Insert(v); e != nil {
		t.Fatal(e)
	}
	service := Service{Store: s}
	got, obs, e := service.Refresh(context.Background(), v)
	if e != nil || obs.State != "missing" || got.Status != "interrupted" {
		t.Fatalf("%+v %+v %v", got, obs, e)
	}
	if e = s.Finish(v.ID, 0, ""); e != nil {
		t.Fatal(e)
	}
	// An inspector holding an old snapshot must not overwrite a recorded exit.
	got, _, e = service.Refresh(context.Background(), v)
	if e != nil || got.Status != "stopped" {
		t.Fatal(got, e)
	}
}
func TestRexMetadataRecovery(t *testing.T) {
	v := taskFixture()
	v.Session.Backend = "rex"
	v.Session.Name = "paddock-" + v.ID
	v.Session.TerminalPath = script(t, `case "$*" in
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:real","label":"`+v.Session.Name+`"}]}' ;;
 *'inspect session:real --json') printf '%s' '{"windows":[{"window_id":"window:real","focused_block_id":"block:real"}]}' ;;
 *'process') printf '%s' '{"child":{"pid":42},"last_exit":null}' ;;
 *) exit 1 ;;
esac
`)
	session, obs := terminalFor(v.Session).Inspect(context.Background(), v)
	if obs.State != "running" || session.ID != "session:real" || session.BlockID != "block:real" || session.WindowID != "window:real" {
		t.Fatalf("%+v %+v", session, obs)
	}
}
func TestRexAttachUsesClientAction(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	v := taskFixture()
	v.Session.Backend = "rex"
	v.Session.ID = "session:real"
	v.Session.BlockID = "block:real"
	v.Session.WindowID = "window:real"
	v.Session.TerminalPath = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:real","label":"task"}]}' ;;
 *'process') printf '%s' '{"last_exit":{"exit_code":0}}' ;;
 *'actions --json') printf '%s' '{"actions":[{"name":"session.select"}]}' ;;
 *'do session.select'*) exit 0 ;;
 *) exit 1 ;;
esac
`)
	if e := terminalFor(v.Session).Attach(context.Background(), v, "client:chosen"); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(log)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), "-C client:chosen do session.select session_id=session:real window_id=window:real") {
		t.Fatal(string(b))
	}
}

func TestRexAttachSelectsInvokingClient(t *testing.T) {
	const apps = `{"clients":[
		{"client_id":"client:mini","connection_state":"connected","info":{"kind":"app","label":"Mini"}},
		{"client_id":"client:laptop","connection_state":"connected","info":{"kind":"app","label":"Laptop"}},
		{"client_id":"client:cli","connection_state":"connected","info":{"kind":"cli"}},
		{"client_id":"client:offline","connection_state":"disconnected","info":{"kind":"app"}}
	]}`
	const sole = `{"clients":[{"client_id":"client:mini","connection_state":"connected","info":{"kind":"app"}}]}`
	for _, tc := range []struct {
		name, explicit, envClient, block, session, clients, owner, inspectJSON, want string
		inspectFails, inspect, ambiguous                                             bool
	}{
		{name: "invoking laptop", block: "block:caller", session: "session:caller", owner: "client:laptop", inspect: true, want: "client:laptop"},
		{name: "invoking mini", block: "block:caller", session: "session:caller", owner: "client:mini", inspect: true, want: "client:mini"},
		{name: "explicit overrides environment and pane", explicit: "client:chosen", envClient: "client:mini", block: "block:caller", owner: "client:laptop", want: "client:chosen"},
		{name: "environment overrides pane", envClient: "client:mini", block: "block:caller", owner: "client:laptop", want: "client:mini"},
		{name: "block without session", block: "block:caller", owner: "client:laptop", inspect: true, want: "client:laptop"},
		{name: "outside Rex", ambiguous: true},
		{name: "session without block", session: "session:caller", ambiguous: true},
		{name: "stale pane", block: "block:caller", inspectFails: true, inspect: true, ambiguous: true},
		{name: "unowned pane", block: "block:caller", inspect: true, ambiguous: true},
		{name: "disconnected owner", block: "block:caller", owner: "client:offline", inspect: true, ambiguous: true},
		{name: "CLI owner", block: "block:caller", owner: "client:cli", inspect: true, ambiguous: true},
		{name: "unknown owner", block: "block:caller", owner: "client:unknown", inspect: true, ambiguous: true},
		{name: "malformed pane response", block: "block:caller", inspectJSON: "invalid", inspect: true, ambiguous: true},
		{name: "wrong pane response", block: "block:caller", inspectJSON: `{"block":{"block_id":"block:other","owner":"client:laptop"}}`, inspect: true, ambiguous: true},
		{name: "sole app outside Rex", clients: sole, want: "client:mini"},
		{name: "sole app with stale pane", clients: sole, block: "block:stale", inspectFails: true, want: "client:mini"},
		{name: "no connected apps", clients: `{"clients":[]}`, ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REX_CLIENT", tc.envClient)
			t.Setenv("REX_BLOCK", tc.block)
			t.Setenv("REX_SESSION", tc.session)
			clients := tc.clients
			if clients == "" {
				clients = apps
			}
			inspection := tc.inspectJSON
			if inspection == "" {
				b, err := json.Marshal(map[string]any{"block": map[string]string{"block_id": tc.block, "owner": tc.owner}})
				if err != nil {
					t.Fatal(err)
				}
				inspection = string(b)
			}
			inspectCommand := "printf '%s' " + quote(inspection)
			if tc.inspectFails {
				inspectCommand = "exit 1"
			}
			log := filepath.Join(t.TempDir(), "calls")
			v := taskFixture()
			v.Session.Backend = "rex"
			v.Session.ID = "session:task"
			v.Session.BlockID = "block:task"
			v.Session.WindowID = "window:task"
			v.Session.TerminalPath = script(t, `printf '%s\n' "$*" >> `+quote(log)+`
case "$*" in
 *'client ls --json') printf '%s' `+quote(clients)+` ;;
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:task"}]}' ;;
 *'process') printf '%s' '{"last_exit":{"exit_code":0}}' ;;
 *'block inspect '*) `+inspectCommand+` ;;
 *'actions --json') printf '%s' '{"actions":[{"name":"session.select"}]}' ;;
 *'do session.select'*) exit 0 ;;
 *) exit 1 ;;
esac
`)
			err := terminalFor(v.Session).Attach(context.Background(), v, tc.explicit)
			calls, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.ambiguous {
				if err == nil || !strings.Contains(err.Error(), "choose a connected Rex app") {
					t.Fatalf("wanted ambiguity error, got %v", err)
				}
				if strings.Contains(string(calls), "session.select") {
					t.Fatalf("selected an app despite ambiguity: %s", calls)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				want := "-C " + tc.want + " do session.select session_id=session:task window_id=window:task"
				if !strings.Contains(string(calls), want) {
					t.Fatalf("missing %q in calls: %s", want, calls)
				}
			}
			if got := strings.Contains(string(calls), "block inspect "); got != tc.inspect {
				t.Fatalf("inspected invoking pane = %v, want %v: %s", got, tc.inspect, calls)
			}
			if tc.inspect {
				want := "block inspect " + tc.block + " --json"
				if tc.session != "" {
					want += " -s " + tc.session
				}
				if !strings.Contains(string(calls), want) {
					t.Fatalf("missing caller inspection %q: %s", want, calls)
				}
			}
		})
	}
}

func TestRexMissingPaneRequiresConfirmedListing(t *testing.T) {
	for _, tc := range []struct{ listing, state string }{
		{`{"blocks":[]}`, "missing"},
		{`{"blocks":[{"block_id":"block:replacement"}]}`, "unknown"},
		{`{"blocks":[{"block_id":"block:owned"}]}`, "unknown"},
		{`{"blocks":[{}]}`, "unknown"},
		{`{}`, "unknown"},
		{`invalid`, "unknown"},
	} {
		v := taskFixture()
		v.Session.Backend = "rex"
		v.Session.ID = "session:real"
		v.Session.BlockID = "block:owned"
		v.Session.TerminalPath = script(t, `case "$*" in
 *'block ls '*) printf '%s' '`+tc.listing+`' ;;
 *'ls --json') printf '%s' '{"sessions":[{"session_id":"session:real"}]}' ;;
 *) exit 1 ;;
esac
`)
		_, obs := terminalFor(v.Session).Inspect(context.Background(), v)
		if obs.State != tc.state {
			t.Fatalf("listing %s: got %+v", tc.listing, obs)
		}
	}
}
