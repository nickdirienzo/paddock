package paddock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// One atomic task ledger owns revisions, idempotency and winning decisions.
// Message receipts live separately and are recovered through deterministic keys.
type accessTaskState struct {
	SchemaVersion int                              `json:"schema_version"`
	TaskID        string                           `json:"task_id"`
	Requests      map[string]*accessStoredRequest  `json:"requests"`
	Decisions     map[string]*accessStoredDecision `json:"decisions"`
	Idempotency   map[string]string                `json:"idempotency"`
	Advisor       AccessAdvisor                    `json:"advisor"`
	CheckedAt     string                           `json:"checked_at"`
	TargetBlock   *accessTargetBlock               `json:"target_block,omitempty"`
}

// Known target loss is durable: a later network-only failure must not restore
// notification delivery using the same cached registry identity.
type accessTargetBlock struct {
	Target AccessTarget `json:"target"`
	Error  AccessError  `json:"error"`
}
type accessStoredRequest struct {
	View                     AccessView                      `json:"view"`
	Snapshots                map[string]accessStoredSnapshot `json:"snapshots"`
	Winner                   string                          `json:"winner"`
	CreatedAt                string                          `json:"created_at,omitempty"`
	NotifiedAt               string                          `json:"notified_at,omitempty"`
	DesktopNotificationError string                          `json:"desktop_notification_error,omitempty"`
}
type accessStoredSnapshot struct {
	Snapshot    AccessSnapshot `json:"snapshot"`
	ReviewToken string         `json:"review_token"`
}
type accessStoredDecision struct {
	Receipt             AccessReceipt       `json:"receipt"`
	Input               AccessDecisionInput `json:"input"`
	BackendRequestID    string              `json:"backend_request_id"`
	BackendAcknowledged bool                `json:"backend_acknowledged"`
	NotificationBody    string              `json:"notification_body"`
	ConversationID      string              `json:"conversation_id"`
}

func emptyAccessState(taskID string) accessTaskState {
	return accessTaskState{SchemaVersion: 1, TaskID: taskID, Requests: map[string]*accessStoredRequest{}, Decisions: map[string]*accessStoredDecision{}, Idempotency: map[string]string{}, Advisor: AccessAdvisor{State: "unknown"}}
}
func (s *Store) accessStatePath(taskID string) string {
	return filepath.Join(s.Home, "access-v1", taskID+".json")
}
func (s *Store) readAccessState(taskID string) (accessTaskState, error) {
	v := emptyAccessState(taskID)
	if !validID(taskID) {
		return v, fmt.Errorf("invalid access task ID")
	}
	b, err := os.ReadFile(s.accessStatePath(taskID))
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if len(b) > 64<<20 {
		return v, fmt.Errorf("access ledger exceeds safety limit")
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return v, err
	}
	if v.SchemaVersion != 1 || v.TaskID != taskID || v.Requests == nil || v.Decisions == nil || v.Idempotency == nil {
		return v, fmt.Errorf("invalid access ledger identity/version")
	}
	for id, r := range v.Requests {
		if r == nil || !validAccessID(id) || r.View.ID != id || r.View.TaskID != taskID || r.View.Kind != "network" || r.Snapshots == nil {
			return v, fmt.Errorf("invalid persisted access request")
		}
		for revision, snapshot := range r.Snapshots {
			h, e := accessDigest(snapshot.Snapshot)
			if e != nil || h != revision || snapshot.Snapshot.TaskID != taskID || snapshot.Snapshot.RequestID != id {
				return v, fmt.Errorf("invalid persisted access snapshot")
			}
			if snapshot.Snapshot.Kind == "network" && snapshot.ReviewToken == "" {
				return v, fmt.Errorf("missing private review token")
			}
			if snapshot.ReviewToken != "" {
				tok, _ := accessDigest(snapshot.ReviewToken)
				if tok != snapshot.Snapshot.ReviewTokenDigest {
					return v, fmt.Errorf("invalid stored review token")
				}
			}
		}
		if r.View.Revision != "" {
			snapshot, ok := r.Snapshots[r.View.Revision]
			if !ok || r.View.Snapshot == nil {
				return v, fmt.Errorf("current access revision is missing")
			}
			h, e := accessDigest(r.View.Snapshot)
			if e != nil || h != r.View.Revision || snapshot.Snapshot.Target != r.View.Target {
				return v, fmt.Errorf("current access view does not match immutable revision")
			}
		}
		if r.Winner != "" && v.Decisions[r.Winner] == nil {
			return v, fmt.Errorf("missing access winner")
		}
	}
	for id, d := range v.Decisions {
		if d == nil || !validID(id) || d.Receipt.ID != id || d.Receipt.TaskID != taskID || d.Input.TaskID != taskID || d.Receipt.ProtocolVersion != 1 {
			return v, fmt.Errorf("invalid persisted access decision")
		}
		r := v.Requests[d.Receipt.RequestID]
		if r == nil || d.Receipt.ExternalID != d.Input.ExternalID || d.Receipt.Revision != d.Input.Revision || v.Idempotency[accessKey(d.Receipt.Decision.Caller, d.Input.ExternalID)] != id {
			return v, fmt.Errorf("access decision intent/index identity changed")
		}
		switch d.Receipt.Decision.State {
		case "claimed", "applying", "confirmed", "uncertain":
			if r.Winner != id {
				return v, fmt.Errorf("accepted access intent lost its winner")
			}
		case "stale", "conflict":
		default:
			return v, fmt.Errorf("unknown persisted access decision state")
		}
	}
	for key, id := range v.Idempotency {
		d := v.Decisions[id]
		if d == nil || key != accessKey(d.Receipt.Decision.Caller, d.Input.ExternalID) {
			return v, fmt.Errorf("dangling access decision intent index")
		}
	}
	if err := s.initializeAccessMetadata(&v); err != nil {
		return v, err
	}
	return v, nil
}
func (s *Store) writeAccessState(v accessTaskState) error {
	if !validID(v.TaskID) {
		return fmt.Errorf("invalid access task ID")
	}
	if err := os.MkdirAll(filepath.Dir(s.accessStatePath(v.TaskID)), 0700); err != nil {
		return err
	}
	if err := s.initializeAccessMetadata(&v); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > 64<<20 {
		return fmt.Errorf("access ledger exceeds safety limit")
	}
	return writeJSONAtomic(s.accessStatePath(v.TaskID), v)
}
func (s *Store) updateAccessState(taskID string, fn func(*accessTaskState) error) error {
	return s.withLock(func() error {
		v, err := s.readAccessState(taskID)
		if err != nil {
			return err
		}
		if err = fn(&v); err != nil {
			return err
		}
		return s.writeAccessState(v)
	})
}
func (s *Store) accessState(taskID string) (accessTaskState, error) {
	var v accessTaskState
	err := s.withLock(func() error { var e error; v, e = s.readAccessState(taskID); return e })
	return v, err
}

func accessKey(caller, externalID string) string {
	h, _ := accessDigest([]string{caller, externalID})
	return h
}
func accessTargetFor(task Task) AccessTarget {
	if task.OpenShell == nil {
		return AccessTarget{}
	}
	return AccessTarget{task.OpenShell.ID, task.OpenShell.Name, task.OpenShell.Config.Gateway, task.OpenShell.Config.Workspace}
}
func accessStateViews(state accessTaskState, fresh bool) []AccessView {
	views := make([]AccessView, 0, len(state.Requests))
	for _, stored := range state.Requests {
		view := stored.View
		view.Receipt = nil
		if stored.Winner != "" {
			r := state.Decisions[stored.Winner].Receipt
			view.Receipt = &r
		}
		view.Actionable = fresh && view.Status == "pending" && stored.Winner == "" && len(view.SupportedDecisions) > 0 && view.Snapshot != nil
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return views
}

// Recovery uses MessageReceipt as authority, never a stale copied delivery bit.
func (s *Store) accessNotificationView(d accessStoredDecision) (AccessNotification, error) {
	n := d.Receipt.Notification
	if n.ExternalID == "" {
		return n, nil
	}
	if n.Status == "unavailable" {
		return n, nil
	}
	b, err := os.ReadFile(s.messageReceiptPath(d.Receipt.TaskID, accessNotificationCaller, n.ExternalID))
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return n, err
	}
	var r MessageReceipt
	if err = json.Unmarshal(b, &r); err != nil {
		return n, err
	}
	if r.Version != 1 || r.TaskID != d.Receipt.TaskID || r.Caller != accessNotificationCaller || r.ExternalID != n.ExternalID || r.Body != d.NotificationBody {
		return n, fmt.Errorf("access message receipt identity changed")
	}
	n.Status, n.ThreadID = r.Status, r.ThreadID
	n.Retryable = r.Status == "pending"
	n.Error = nil
	if r.Error != "" {
		n.Error = accessError("notification_"+r.Status, "Native decision notification is "+r.Status+"; inspect before retrying ambiguous delivery.", n.Retryable)
	}
	if r.Status != "pending" && r.Status != "claimed" && r.Status != "delivered" && r.Status != "uncertain" {
		return n, fmt.Errorf("invalid notification status")
	}
	return n, nil
}
func (s *Store) refreshAccessReceiptViews(v *accessTaskState) error {
	for _, d := range v.Decisions {
		n, err := s.accessNotificationView(*d)
		if err != nil {
			return err
		}
		d.Receipt.Notification = n
	}
	return nil
}

func (s Service) noPendingAccessDecisions(taskID string) error {
	v, err := s.Store.accessState(taskID)
	if err != nil {
		return err
	}
	if err = s.Store.refreshAccessReceiptViews(&v); err != nil {
		return err
	}
	for _, r := range v.Requests {
		if r.View.Status == "pending" || r.View.Status == "unknown" {
			return fmt.Errorf("pending or unknown access request")
		}
	}
	for _, d := range v.Decisions {
		if d.Receipt.Decision.State == "claimed" || d.Receipt.Decision.State == "applying" || d.Receipt.Decision.State == "uncertain" {
			return fmt.Errorf("pending or uncertain access decision")
		}
		switch d.Receipt.Notification.Status {
		case "pending", "claimed", "uncertain":
			return fmt.Errorf("pending or uncertain access notification")
		}
	}
	return nil
}

func (s Service) accessMessageDisposed(r MessageReceipt) bool {
	if r.Caller != accessNotificationCaller || !strings.HasPrefix(r.ExternalID, "access:") {
		return false
	}
	id := strings.TrimPrefix(r.ExternalID, "access:")
	if !validID(id) {
		return false
	}
	v, err := s.Store.accessState(r.TaskID)
	if err != nil {
		return false
	}
	d := v.Decisions[id]
	return d != nil && d.Receipt.Notification.Status == "unavailable" && d.NotificationBody == r.Body && d.Receipt.Notification.ExternalID == r.ExternalID
}
