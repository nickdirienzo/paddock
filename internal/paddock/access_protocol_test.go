package paddock

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAccessDecisionInputStrictAndBounded(t *testing.T) {
	s, task, _, requestID := accessReviewFixture(t)
	show := s.ShowAccessV1(context.Background(), task.ID, requestID, true)
	in := AccessDecisionInput{ProtocolVersion: 1, TaskID: task.ID, Revision: show.Request.Revision, Decision: "approve", ExternalID: "strict-input", Actor: AccessActor{Source: "arnold-slack", WorkspaceID: "T1", UserID: "U1"}, Reason: "Read dependencies"}
	b, _ := json.Marshal(in)
	got, err := DecodeAccessDecision(b)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatal(got, err)
	}
	bad := [][]byte{append(append([]byte{}, b...), b...), bytes.Replace(b, []byte(`"protocol_version":1`), []byte(`"protocol_version":1,"protocol_version":1`), 1), bytes.Replace(b, []byte(`"protocol_version":1`), []byte(`"protocol_version":2`), 1), bytes.Replace(b, []byte(`"reason":"Read dependencies"`), []byte(`"command":"sh arbitrary"`), 1), []byte(strings.Repeat(" ", maxAccessInput+1)), {0xff}}
	for _, data := range bad {
		if _, err := DecodeAccessDecision(data); err == nil {
			t.Fatalf("accepted ambiguous or unsupported input: %.100s", data)
		}
	}
	path := filepath.Join(t.TempDir(), "decision.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadAccessDecisionFile(path); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadAccessDecisionFile(link); err == nil {
		t.Fatal("accepted symlink decision input")
	}
}

func TestAccessDigestPreservesCompleteRuleAndIntegerPrecision(t *testing.T) {
	a := json.RawMessage(`{"n":9007199254740992,"rules":[{"allow":{"method":"GET","path":"/a"}}]}`)
	b := json.RawMessage(`{"rules":[{"allow":{"path":"/a","method":"GET"}}],"n":9007199254740992}`)
	c := json.RawMessage(`{"n":9007199254740993,"rules":[{"allow":{"method":"GET","path":"/a"}}]}`)
	ha, _ := accessDigest(a)
	hb, _ := accessDigest(b)
	hc, _ := accessDigest(c)
	if ha != hb || ha == hc {
		t.Fatal("canonical digest lost object equivalence or integer precision")
	}
}

func TestAccessProtocolOwnerAndFreshness(t *testing.T) {
	s, task, backend, id := accessReviewFixture(t)
	cached := s.ListAccessV1(context.Background(), task.ID, false)
	if cached.ProtocolVersion != 1 || cached.Refresh.State != "cached" || len(cached.Features) == 0 {
		t.Fatal(cached)
	}
	fresh := s.ShowAccessV1(context.Background(), task.ID, id, true)
	if fresh.Error != nil || !fresh.Request.Actionable {
		t.Fatal(fresh)
	}
	backend.inspection.ExactRuleBound = false
	closed := s.ShowAccessV1(context.Background(), task.ID, id, true)
	if closed.Error != nil || closed.Request.Actionable || len(closed.Request.SupportedDecisions) != 0 || closed.Request.Error.Code != "precondition_unavailable" {
		t.Fatal(closed)
	}
	task.ID = newID()
	task.Provenance.Caller = "999:another-service"
	if err := s.Store.Insert(task); err != nil {
		t.Fatal(err)
	}
	denied := s.ShowAccessV1(context.Background(), task.ID, id, false)
	if denied.Error == nil || denied.Error.Code != "unauthorized" || denied.Request != nil {
		t.Fatal(denied)
	}
}

func TestAccessExportedWireExamples(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "access-wire-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion   int                        `json:"schema_version"`
		ProtocolVersion int                        `json:"protocol_version"`
		Examples        map[string]json.RawMessage `json:"exported_examples"`
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 || doc.ProtocolVersion != 1 {
		t.Fatal("schema/protocol versions drifted")
	}
	if _, err = DecodeAccessDecision(doc.Examples["decision_input"]); err != nil {
		t.Fatal(err)
	}
	checkReceipt := func(name string, receipt *AccessReceipt) {
		t.Helper()
		if receipt == nil {
			return
		}
		if !validID(receipt.ID) {
			t.Fatalf("exported %s receipt ID must be 24 lowercase hex characters: %q", name, receipt.ID)
		}
		if receipt.Notification.ExternalID != "" && receipt.Notification.ExternalID != "access:"+receipt.ID {
			t.Fatalf("exported %s notification key must bind the exact receipt ID", name)
		}
	}
	// Exact round trips catch missing/renamed exported fields and null-vs-omitted
	// drift without a second independently maintained wire representation.
	values := map[string]any{"list": &AccessListResponse{}, "show": &AccessShowResponse{}, "receipt": &AccessReceipt{}, "decision_response": &AccessDecisionResponse{}, "uncertain_response": &AccessDecisionResponse{}, "no_decision_response": &AccessDecisionResponse{}}
	for name, value := range values {
		if err = json.Unmarshal(doc.Examples[name], value); err != nil {
			t.Fatal(name, err)
		}
		switch value := value.(type) {
		case *AccessReceipt:
			checkReceipt(name, value)
		case *AccessDecisionResponse:
			checkReceipt(name, value.Receipt)
		case *AccessListResponse:
			for _, request := range value.Requests {
				checkReceipt(name, request.Receipt)
			}
		case *AccessShowResponse:
			if value.Request != nil {
				checkReceipt(name, value.Request.Receipt)
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var before, after any
		_ = json.Unmarshal(doc.Examples[name], &before)
		_ = json.Unmarshal(encoded, &after)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("exported wire example %s does not match Go fields", name)
		}
	}
	var absent AccessDecisionResponse
	if err := json.Unmarshal(doc.Examples["no_decision_response"], &absent); err != nil || absent.Receipt != nil || absent.Error == nil || absent.Error.Code != "no_decision" || !absent.Error.Retryable {
		t.Fatal("invalid no-decision recovery example", absent, err)
	}
}
