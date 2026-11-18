package paddock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// AccessRequest is the legacy operator projection. Structured proposals,
// decisions and notification metadata are stored only in access-v1 ledgers.
type AccessRequest struct {
	ID                       string   `json:"id"`
	TaskID                   string   `json:"task_id"`
	Sandbox                  string   `json:"sandbox"`
	Status                   string   `json:"status"`
	Rule                     string   `json:"rule,omitempty"`
	Binary                   string   `json:"binary,omitempty"`
	Confidence               string   `json:"confidence,omitempty"`
	Rationale                string   `json:"rationale,omitempty"`
	Prover                   string   `json:"prover,omitempty"`
	Candidate                string   `json:"candidate,omitempty"`
	Endpoints                string   `json:"endpoints,omitempty"`
	Binaries                 string   `json:"binaries,omitempty"`
	Hits                     string   `json:"hits,omitempty"`
	Security                 []string `json:"security,omitempty"`
	ApplicationError         string   `json:"application_error,omitempty"`
	DesktopNotificationError string   `json:"desktop_notification_error,omitempty"`
	Decision                 string   `json:"decision,omitempty"`
	DecisionReason           string   `json:"decision_reason,omitempty"`
	DecisionError            string   `json:"decision_error,omitempty"`
	AgentNotificationError   string   `json:"agent_notification_error,omitempty"`
	CreatedAt                string   `json:"created_at"`
	UpdatedAt                string   `json:"updated_at"`
	NotifiedAt               string   `json:"notified_at,omitempty"`
	DecidedAt                string   `json:"decided_at,omitempty"`
}

type accessRecord struct {
	Version int `json:"version"`
	AccessRequest
}

func validAccessID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func (s *Store) getAccess(prefix string) (AccessRequest, error) {
	if prefix == "" {
		return AccessRequest{}, fmt.Errorf("access request ID is required")
	}
	old, err := s.readLegacyAccessRequests("")
	if err != nil {
		return AccessRequest{}, err
	}
	requests, err := s.mergeModernAccess(old, "", "")
	if err != nil {
		return AccessRequest{}, err
	}
	var found AccessRequest
	for _, r := range requests {
		if !strings.HasPrefix(r.ID, prefix) {
			continue
		}
		if found.ID != "" {
			return found, fmt.Errorf("access request prefix %q is ambiguous", prefix)
		}
		found = r
	}
	if found.ID == "" {
		return found, fmt.Errorf("access request %q not found", prefix)
	}
	return found, nil
}

func (s *Store) GetAccess(prefix string) (AccessRequest, error) {
	var request AccessRequest
	err := s.withLock(func() error { var err error; request, err = s.getAccess(prefix); return err })
	return request, err
}

func (s *Store) ListAccess(taskID, status string) ([]AccessRequest, error) {
	requests := []AccessRequest{}
	err := s.withLock(func() error {
		old, err := s.readLegacyAccessRequests(taskID)
		if err != nil {
			return err
		}
		requests, err = s.mergeModernAccess(old, taskID, status)
		return err
	})
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].CreatedAt == requests[j].CreatedAt {
			return requests[i].ID < requests[j].ID
		}
		return requests[i].CreatedAt < requests[j].CreatedAt
	})
	return requests, err
}

// SyncAccess refreshes the same structured ledger used by integration decisions.
// Its result contains pending desktop-notification candidates, not a second inbox.
func (s Service) SyncAccess(ctx context.Context, task Task) ([]AccessRequest, error) {
	response := s.ListAccessV1(ctx, task.ID, true)
	if response.Error != nil {
		return nil, response.Error
	}
	if response.Refresh.State != "fresh" {
		return nil, fmt.Errorf("access inspection was not fresh")
	}
	pending := map[string]bool{}
	for _, r := range response.Requests {
		if r.Status == "pending" {
			pending[r.ID] = true
		}
	}
	projected, err := s.Store.ListAccess(task.ID, "pending")
	if err != nil {
		return nil, err
	}
	out := []AccessRequest{}
	for _, r := range projected {
		if pending[r.ID] && r.NotifiedAt == "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// Claim once before desktop delivery. A crash can suppress an informational
// notification, but concurrent pollers cannot send duplicate alerts. Grant and
// native-message recovery remain independently durable.
func (s *Store) claimAccessDesktop(taskID string, requests []AccessRequest) ([]AccessRequest, error) {
	claimed := []AccessRequest{}
	err := s.updateAccessState(taskID, func(state *accessTaskState) error {
		for _, request := range requests {
			r := state.Requests[request.ID]
			if r == nil || r.View.Status != "pending" || r.NotifiedAt != "" {
				continue
			}
			r.NotifiedAt = now()
			r.DesktopNotificationError = ""
			claimed = append(claimed, legacyAccessView(*state, r, AccessRequest{}))
		}
		return nil
	})
	return claimed, err
}

func (s Service) SyncRunningAccess(ctx context.Context) error {
	tasks, err := s.Store.List("")
	if err != nil {
		return err
	}
	var failures []error
	for _, task := range tasks {
		if task.RuntimeName() != "openshell" || task.OpenShell == nil || !task.OpenShell.Ready || (task.Status != "running" && task.Status != "starting") {
			continue
		}
		requests, err := s.SyncAccess(ctx, task)
		if err != nil {
			failures = append(failures, fmt.Errorf("task %s: %w", task.ID, err))
			continue
		}
		if len(requests) == 0 {
			continue
		}
		requests, err = s.Store.claimAccessDesktop(task.ID, requests)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if len(requests) == 0 {
			continue
		}
		notifyErr := notifyAccessRequests(task, requests)
		if err = s.Store.updateAccessState(task.ID, func(state *accessTaskState) error {
			for _, request := range requests {
				if r := state.Requests[request.ID]; r != nil && r.NotifiedAt == request.NotifiedAt {
					r.DesktopNotificationError = ""
					if notifyErr != nil {
						r.DesktopNotificationError = "Desktop notification failed; inspect pending access on the host."
					}
				}
			}
			return nil
		}); err != nil {
			failures = append(failures, err)
		}
		if notifyErr != nil {
			failures = append(failures, fmt.Errorf("task %s desktop access notification failed", task.ID))
		}
	}
	return errors.Join(failures...)
}

func notifyAccessRequests(task Task, requests []AccessRequest) error {
	if len(requests) == 0 || os.Getenv("PADDOCK_DESKTOP_NOTIFICATIONS") == "0" || runtime.GOOS != "darwin" {
		return nil
	}
	hosts := make([]string, 0, len(requests))
	for _, request := range requests {
		endpoint := strings.Fields(request.Endpoints)
		if len(endpoint) > 0 {
			hosts = append(hosts, endpoint[0])
		}
	}
	if len(hosts) > 3 {
		hosts = append(hosts[:3], fmt.Sprintf("+%d more", len(hosts)-3))
	}
	body := fmt.Sprintf("Task %s requests %d network rule(s): %s. Run paddock access pending.", task.ID[:12], len(requests), strings.Join(hosts, ", "))
	script := `on run argv
display notification (item 2 of argv) with title (item 1 of argv)
end run`
	return exec.Command("/usr/bin/osascript", "-e", script, "--", "Paddock access request", body).Run()
}

// The operator API is a wrapper over the same guarded executor. It never writes
// a legacy record or grants from a historical display projection.
func (s Service) DecideAccess(ctx context.Context, prefix, decision, reason string) (AccessRequest, error) {
	request, err := s.Store.GetAccess(prefix)
	if err != nil {
		return request, err
	}
	if decision != "approve" && decision != "reject" {
		return request, fmt.Errorf("invalid access decision")
	}
	if decision == "reject" && strings.TrimSpace(reason) == "" {
		return request, fmt.Errorf("a rejection reason is required")
	}
	show := s.ShowAccessV1(ctx, request.TaskID, request.ID, true)
	if show.Error != nil {
		return request, show.Error
	}
	if show.Request == nil || show.Request.Status != "pending" {
		return request, fmt.Errorf("access request is no longer pending")
	}
	caller, err := LocalCaller()
	if err != nil {
		return request, err
	}
	input := AccessDecisionInput{ProtocolVersion: 1, TaskID: request.TaskID, Revision: show.Request.Revision, Decision: decision, ExternalID: "operator:" + newID(), Actor: AccessActor{Source: "local-operator", UserID: caller}, Reason: reason}
	response := s.DecideAccessV1(ctx, request.ID, input)
	if response.Receipt != nil {
		projected, err := s.Store.GetAccess(request.ID)
		if err != nil {
			return request, err
		}
		request = projected
	}
	if response.Error != nil {
		return request, response.Error
	}
	return request, nil
}

func (s Service) noPendingAccess(id string) error {
	requests, err := s.Store.ListAccess(id, "pending")
	if err != nil {
		return err
	}
	if len(requests) > 0 {
		return fmt.Errorf("pending OpenShell access request")
	}
	return nil
}

const accessPollInterval = 10 * time.Second
