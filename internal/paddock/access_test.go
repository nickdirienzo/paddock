package paddock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const accessFixture = `Network Rules:  (version 4, 2 chunks)

  Chunk: 4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89
  Status: pending
  Rule: allow_pypi_org_443
  Binary: /opt/uv
  Confidence: 75%
  Rationale: Allow uv to connect to pypi.org:443 (HTTPS).
  Prover: prover: no new findings
  Candidate: 2bcf80473a74
  Endpoints: pypi.org:443 [L4]
  Binaries: /opt/uv
  Hits: 14 (first seen today)

  Chunk: ef5aaeb0-8a88-4430-be71-09eccbb10b19
  Status: pending
  Rule: allow_api_github_com_443
  Binary: /usr/bin/codex
  Confidence: 65%
  Rationale: Allow codex to connect to api.github.com:443 (HTTPS).
  Security: credential reach expansion
  Application: candidate canonicalization failed
  Endpoints: api.github.com:443 [L4]
  Binaries: /usr/bin/codex
`

func accessTaskFixture() Task {
	task := taskFixture()
	task.Runtime = "openshell"
	task.Status = "running"
	task.OpenShell = &OpenShellTask{Name: "pd-fixture", Ready: true, Config: OpenShellConfig{Binary: "/bin/false", Gateway: "fixture", Workspace: "default"}}
	task.Session.Agent = "unsupported"
	return task
}

func TestParseOpenShellRules(t *testing.T) {
	task := accessTaskFixture()
	requests, err := parseOpenShellRules([]byte(accessFixture), task)
	if err != nil || len(requests) != 2 {
		t.Fatalf("%+v %v", requests, err)
	}
	if requests[0].ID != "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89" || requests[0].TaskID != task.ID || requests[0].Endpoints != "pypi.org:443 [L4]" || requests[0].Hits == "" {
		t.Fatalf("wrong first proposal: %+v", requests[0])
	}
	if requests[1].ApplicationError != "candidate canonicalization failed" || len(requests[1].Security) != 1 {
		t.Fatalf("lost review warnings: %+v", requests[1])
	}
	if _, err = parseOpenShellRules([]byte("  Chunk: not-a-uuid\n  Status: pending\n"), task); err == nil {
		t.Fatal("accepted unsafe proposal identity")
	}
}

func TestAccessPersistenceAndDecision(t *testing.T) {
	home := t.TempDir()
	store := openTestStore(t, home)
	task := accessTaskFixture()
	state := filepath.Join(t.TempDir(), "state")
	calls := filepath.Join(t.TempDir(), "calls")
	bin := script(t, `
state=`+quote(state)+`
calls=`+quote(calls)+`
case "$5 $6" in
  'rule get')
    status=pending
    if test -f "$state"; then status=$(cat "$state"); fi
    sed "s/Status: pending/Status: $status/g" <<'EOF'
`+accessFixture+`EOF
    ;;
  'rule approve')
    printf '%s\n' "$*" >> "$calls"
    printf approved > "$state"
    ;;
  'rule reject')
    printf '%s\n' "$*" >> "$calls"
    printf rejected > "$state"
    ;;
esac
`)
	task.OpenShell.Config.Binary = bin
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	newRequests, err := service.SyncAccess(context.Background(), task)
	if err != nil || len(newRequests) != 2 {
		t.Fatalf("%+v %v", newRequests, err)
	}
	if again, err := service.SyncAccess(context.Background(), task); err != nil || len(again) != 0 {
		t.Fatalf("sync was not idempotent: %+v %v", again, err)
	}
	request, err := store.GetAccess("4cf929e5")
	if err != nil || request.Status != "pending" {
		t.Fatal(request, err)
	}
	if err = service.noPendingAccess(task.ID); err == nil {
		t.Fatal("pending access did not protect task retention")
	}
	request, err = service.DecideAccess(context.Background(), request.ID, "approve", "")
	if err != nil || request.Status != "approved" || request.DecidedAt == "" {
		t.Fatalf("decision not reconciled: %+v %v", request, err)
	}
	if request.AgentNotificationError == "" {
		t.Fatal("missing durable agent notification failure")
	}
	b, err := os.ReadFile(calls)
	if err != nil || !strings.Contains(string(b), "rule approve pd-fixture --chunk-id "+request.ID) {
		t.Fatalf("wrong OpenShell decision: %s %v", b, err)
	}
	pending, err := store.ListAccess(task.ID, "pending")
	if err != nil || len(pending) != 0 {
		t.Fatalf("wrong pending inbox: %+v %v", pending, err)
	}
	if err = service.noPendingAccess(task.ID); err != nil {
		t.Fatal("resolved access still blocked retention:", err)
	}
}

func TestAccessRejectRequiresReason(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	task := accessTaskFixture()
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	request := AccessRequest{ID: "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89", TaskID: task.ID, Sandbox: task.OpenShell.Name, Status: "pending"}
	if _, _, err := store.upsertAccess(task, request); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{Store: store}).DecideAccess(context.Background(), request.ID, "reject", ""); err == nil {
		t.Fatal("accepted unexplained rejection")
	}
}
