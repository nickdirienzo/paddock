package paddock

import (
	"context"
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
	if e != nil || got.Status != "completed" {
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
