package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// AccessRequest mirrors one OpenShell network-rule proposal. OpenShell remains
// authoritative for the rule and its status; Paddock persists enough metadata
// to provide a durable operator inbox and notification audit.
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

func (s *Store) accessPath(id string) string { return filepath.Join(s.Home, "access", id+".json") }

func (s *Store) readAccess(id string) (AccessRequest, error) {
	var record accessRecord
	if !validAccessID(id) {
		return record.AccessRequest, fmt.Errorf("invalid access request ID %q", id)
	}
	b, err := os.ReadFile(s.accessPath(id))
	if err != nil {
		return record.AccessRequest, err
	}
	if err = json.Unmarshal(b, &record); err != nil {
		return record.AccessRequest, fmt.Errorf("read access request %s: %w", id, err)
	}
	if record.Version != 1 || record.ID != id {
		return record.AccessRequest, fmt.Errorf("access request %s has invalid persisted identity", id)
	}
	return record.AccessRequest, nil
}

func (s *Store) getAccess(prefix string) (AccessRequest, error) {
	if prefix == "" {
		return AccessRequest{}, fmt.Errorf("access request ID is required")
	}
	entries, err := os.ReadDir(filepath.Join(s.Home, "access"))
	if err != nil {
		return AccessRequest{}, err
	}
	id := ""
	for _, entry := range entries {
		candidate := strings.TrimSuffix(entry.Name(), ".json")
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") && strings.HasPrefix(candidate, prefix) {
			if id != "" {
				return AccessRequest{}, fmt.Errorf("access request prefix %q is ambiguous", prefix)
			}
			id = candidate
		}
	}
	if id == "" {
		return AccessRequest{}, fmt.Errorf("access request %q not found", prefix)
	}
	return s.readAccess(id)
}

func (s *Store) GetAccess(prefix string) (AccessRequest, error) {
	var request AccessRequest
	err := s.withLock(func() error { var err error; request, err = s.getAccess(prefix); return err })
	return request, err
}

func (s *Store) ListAccess(taskID, status string) ([]AccessRequest, error) {
	requests := []AccessRequest{}
	err := s.withLock(func() error {
		entries, err := os.ReadDir(filepath.Join(s.Home, "access"))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			request, err := s.readAccess(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return err
			}
			if (taskID == "" || request.TaskID == taskID) && (status == "" || request.Status == status) {
				requests = append(requests, request)
			}
		}
		return nil
	})
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].CreatedAt == requests[j].CreatedAt {
			return requests[i].ID < requests[j].ID
		}
		return requests[i].CreatedAt < requests[j].CreatedAt
	})
	return requests, err
}

func (s *Store) upsertAccess(task Task, incoming AccessRequest) (AccessRequest, bool, error) {
	var saved AccessRequest
	created := false
	err := s.withLock(func() error {
		if task.OpenShell == nil || incoming.Sandbox != task.OpenShell.Name || !validAccessID(incoming.ID) {
			return fmt.Errorf("access request does not match its OpenShell task")
		}
		if _, err := s.read(task.ID); err != nil {
			return err
		}
		existing, err := s.readAccess(incoming.ID)
		if errors.Is(err, os.ErrNotExist) {
			created = true
			incoming.CreatedAt = now()
			existing = incoming
		} else if err != nil {
			return err
		} else {
			if existing.TaskID != task.ID || existing.Sandbox != incoming.Sandbox {
				return fmt.Errorf("access request identity changed")
			}
			createdAt, notifiedAt := existing.CreatedAt, existing.NotifiedAt
			decision, reason, decidedAt := existing.Decision, existing.DecisionReason, existing.DecidedAt
			decisionError, agentError, desktopError := existing.DecisionError, existing.AgentNotificationError, existing.DesktopNotificationError
			existing = incoming
			existing.CreatedAt, existing.NotifiedAt = createdAt, notifiedAt
			existing.Decision, existing.DecisionReason, existing.DecidedAt = decision, reason, decidedAt
			existing.DecisionError, existing.AgentNotificationError = decisionError, agentError
			existing.DesktopNotificationError = desktopError
		}
		existing.UpdatedAt = now()
		if err = writeJSONAtomic(s.accessPath(existing.ID), accessRecord{Version: 1, AccessRequest: existing}); err != nil {
			return err
		}
		saved = existing
		return nil
	})
	return saved, created, err
}

func (s *Store) updateAccess(id string, fn func(*AccessRequest)) error {
	return s.withLock(func() error {
		request, err := s.readAccess(id)
		if err != nil {
			return err
		}
		fn(&request)
		request.UpdatedAt = now()
		return writeJSONAtomic(s.accessPath(request.ID), accessRecord{Version: 1, AccessRequest: request})
	})
}

func parseOpenShellRules(output []byte, task Task) ([]AccessRequest, error) {
	if task.OpenShell == nil {
		return nil, fmt.Errorf("task has no OpenShell sandbox")
	}
	requests := []AccessRequest{}
	var current *AccessRequest
	finish := func() error {
		if current == nil {
			return nil
		}
		if !validAccessID(current.ID) || current.Status == "" {
			return fmt.Errorf("OpenShell returned an incomplete network-rule proposal")
		}
		requests = append(requests, *current)
		current = nil
		return nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Chunk: ") {
			if err := finish(); err != nil {
				return nil, err
			}
			current = &AccessRequest{ID: strings.TrimSpace(strings.TrimPrefix(line, "Chunk: ")), TaskID: task.ID, Sandbox: task.OpenShell.Name}
			continue
		}
		if current == nil || line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "Status":
			current.Status = strings.ToLower(value)
		case "Rule":
			current.Rule = value
		case "Binary":
			current.Binary = value
		case "Confidence":
			current.Confidence = value
		case "Rationale":
			current.Rationale = value
		case "Prover":
			current.Prover = value
		case "Candidate":
			current.Candidate = value
		case "Endpoints":
			current.Endpoints = value
		case "Binaries":
			current.Binaries = value
		case "Hits":
			current.Hits = value
		case "Security":
			current.Security = append(current.Security, value)
		case "Application":
			current.ApplicationError = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return requests, nil
}

// SyncAccess mirrors OpenShell's current proposal view. OpenShell owns the
// actual rule and status; Paddock never synthesizes or broadens a proposal.
func (s Service) SyncAccess(ctx context.Context, task Task) ([]AccessRequest, error) {
	if task.RuntimeName() != "openshell" || task.OpenShell == nil || !task.OpenShell.Ready {
		return nil, fmt.Errorf("task %s has no ready OpenShell sandbox", task.ID)
	}
	output, err := task.OpenShell.Config.call(ctx, "rule", "get", task.OpenShell.Name)
	if err != nil {
		return nil, err
	}
	parsed, err := parseOpenShellRules(output, task)
	if err != nil {
		return nil, err
	}
	newPending := []AccessRequest{}
	for _, request := range parsed {
		saved, created, err := s.Store.upsertAccess(task, request)
		if err != nil {
			return newPending, err
		}
		if created && saved.Status == "pending" {
			newPending = append(newPending, saved)
		}
	}
	return newPending, nil
}

// SyncRunningAccess is used by the shared host poller. Grouping notifications
// by task avoids one desktop alert per CDN redirect or package host.
func (s Service) SyncRunningAccess(ctx context.Context) error {
	tasks, err := s.Store.List("")
	if err != nil {
		return err
	}
	var errs []error
	for _, task := range tasks {
		if task.RuntimeName() != "openshell" || task.OpenShell == nil || !task.OpenShell.Ready || (task.Status != "running" && task.Status != "starting") {
			continue
		}
		requests, syncErr := s.SyncAccess(ctx, task)
		if syncErr != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", task.ID, syncErr))
			continue
		}
		if len(requests) == 0 {
			continue
		}
		notifyErr := notifyAccessRequests(task, requests)
		for _, request := range requests {
			_ = s.Store.updateAccess(request.ID, func(v *AccessRequest) {
				v.NotifiedAt = now()
				v.DesktopNotificationError = ""
				if notifyErr != nil {
					v.DesktopNotificationError = notifyErr.Error()
				}
			})
		}
	}
	return errors.Join(errs...)
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
	cmd := exec.Command("/usr/bin/osascript", "-e", script, "--", "Paddock access request", body)
	return cmd.Run()
}

func (s Service) DecideAccess(ctx context.Context, prefix, decision, reason string) (AccessRequest, error) {
	request, err := s.Store.GetAccess(prefix)
	if err != nil {
		return request, err
	}
	if request.Status != "pending" {
		return request, fmt.Errorf("access request %s is already %s", request.ID, request.Status)
	}
	if decision != "approve" && decision != "reject" {
		return request, fmt.Errorf("invalid access decision %q", decision)
	}
	if decision == "reject" && strings.TrimSpace(reason) == "" {
		return request, fmt.Errorf("a rejection reason is required")
	}
	task, err := s.Store.Get(request.TaskID)
	if err != nil {
		return request, err
	}
	if task.OpenShell == nil || task.OpenShell.Name != request.Sandbox {
		return request, fmt.Errorf("access request no longer matches task sandbox")
	}
	release, err := s.Store.taskLease(task.ID, "operation")
	if err != nil {
		return request, err
	}
	args := []string{"rule", decision, task.OpenShell.Name, "--chunk-id", request.ID}
	if decision == "reject" {
		args = append(args, "--reason", reason)
	}
	_ = s.Store.updateAccess(request.ID, func(v *AccessRequest) {
		v.Decision, v.DecisionReason, v.DecisionError = decision, reason, ""
	})
	_, callErr := task.OpenShell.Config.call(ctx, args...)
	release()
	if callErr != nil {
		_ = s.Store.updateAccess(request.ID, func(v *AccessRequest) { v.DecisionError = callErr.Error() })
		return request, fmt.Errorf("access decision outcome is uncertain: %w; run paddock access show %s before retrying", callErr, request.ID)
	}
	if _, err = s.SyncAccess(ctx, task); err != nil {
		_ = s.Store.updateAccess(request.ID, func(v *AccessRequest) { v.DecisionError = "decision submitted; reconciliation failed: " + err.Error() })
		return request, fmt.Errorf("OpenShell accepted the decision but Paddock could not reconcile it: %w", err)
	}
	request, err = s.Store.GetAccess(request.ID)
	if err != nil {
		return request, err
	}
	expected := "approved"
	if decision == "reject" {
		expected = "rejected"
	}
	if request.Status != expected {
		return request, fmt.Errorf("OpenShell left the proposal %s after revalidation; review it again", request.Status)
	}
	_ = s.Store.updateAccess(request.ID, func(v *AccessRequest) {
		v.DecidedAt, v.DecisionError = now(), ""
	})

	message := "OpenShell network access was approved for " + request.Endpoints + " from " + request.Binary + ". Continue the requested work. Inspect current state before retrying so completed effects are not duplicated, and do not treat the earlier policy denial as evidence against the approach."
	if decision == "reject" {
		message = "OpenShell network access was rejected for " + request.Endpoints + ": " + reason + ". Adapt to that decision and clearly report any resulting limitation."
	}
	if _, messageErr := s.Message(ctx, task, message); messageErr != nil {
		_ = s.Store.updateAccess(request.ID, func(v *AccessRequest) { v.AgentNotificationError = messageErr.Error() })
	}
	return s.Store.GetAccess(request.ID)
}

// Keep GC from removing the only warm checkout while an operator decision is pending.
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
