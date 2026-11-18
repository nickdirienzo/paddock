package paddock

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxLegacyAccessRecords = 4096
const maxLegacyAccessBytes = 1 << 20
const maxLegacyInboxBytes = 16 << 20

// Old access/*.json records remain read-only historical input. They never supply
// snapshots, review tokens, or decision authority. No migration deletes originals.
func (s *Store) readLegacyAccessRequests(taskID string) ([]AccessRequest, error) {
	directory, err := os.Open(filepath.Join(s.Home, "access"))
	if os.IsNotExist(err) {
		return []AccessRequest{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxLegacyAccessRecords + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > maxLegacyAccessRecords {
		return nil, fmt.Errorf("legacy access inbox exceeds compatibility bound")
	}
	requests := []AccessRequest{}
	totalBytes := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !validAccessID(id) {
			return nil, fmt.Errorf("legacy access record has an invalid filename")
		}
		data, err := readAccessRegularFile(filepath.Join(s.Home, "access", name), maxLegacyAccessBytes)
		if err != nil {
			return nil, fmt.Errorf("cannot read bounded legacy access record")
		}
		totalBytes += len(data)
		if totalBytes > maxLegacyInboxBytes {
			return nil, fmt.Errorf("legacy access inbox exceeds byte compatibility bound")
		}
		var record accessRecord
		if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.ID != id || !validID(record.TaskID) || record.Sandbox == "" {
			return nil, fmt.Errorf("legacy access record has an invalid identity or version")
		}
		if taskID == "" || record.TaskID == taskID {
			requests = append(requests, record.AccessRequest)
		}
	}
	return requests, nil
}

func (s *Store) initializeAccessMetadata(state *accessTaskState) error {
	needed := false
	for _, r := range state.Requests {
		if r == nil {
			return fmt.Errorf("invalid access request metadata")
		}
		needed = needed || r.CreatedAt == ""
	}
	if !needed {
		return nil
	}
	legacy, err := s.readLegacyAccessRequests(state.TaskID)
	if err != nil {
		return err
	}
	byID := map[string]AccessRequest{}
	for _, r := range legacy {
		byID[r.ID] = r
	}
	for id, r := range state.Requests {
		if r.CreatedAt != "" {
			continue
		}
		old := byID[id]
		if old.ID != "" && old.Sandbox != r.View.Target.SandboxName {
			return fmt.Errorf("historical access sandbox identity changed")
		}
		r.CreatedAt = old.CreatedAt
		if r.CreatedAt == "" {
			r.CreatedAt = state.CheckedAt
		}
		if r.CreatedAt == "" {
			r.CreatedAt = now()
		}
		r.NotifiedAt, r.DesktopNotificationError = old.NotifiedAt, old.DesktopNotificationError
	}
	return nil
}

func legacyAccessView(state accessTaskState, r *accessStoredRequest, old AccessRequest) AccessRequest {
	old.ID, old.TaskID, old.Sandbox, old.Status = r.View.ID, r.View.TaskID, r.View.Target.SandboxName, r.View.Status
	if r.CreatedAt != "" {
		old.CreatedAt = r.CreatedAt
	} else if old.CreatedAt == "" {
		old.CreatedAt = state.CheckedAt
	}
	old.UpdatedAt = state.CheckedAt
	if r.NotifiedAt != "" {
		old.NotifiedAt, old.DesktopNotificationError = r.NotifiedAt, r.DesktopNotificationError
	}
	if v := r.View.Snapshot; v != nil {
		old.Rule = v.RuleName
		old.Rationale = v.Rationale
		old.Security = v.Security
		old.Prover = v.ValidationResult
		old.ApplicationError = v.ApplicationError
		old.Candidate = v.CandidateEffectivePolicyHash
		old.Confidence, old.Hits = "", ""
		var rule struct {
			Endpoints []struct {
				Host  string   `json:"host"`
				Port  uint32   `json:"port"`
				Ports []uint32 `json:"ports"`
			} `json:"endpoints"`
			Binaries []struct {
				Path string `json:"path"`
			} `json:"binaries"`
		}
		old.Endpoints, old.Binary, old.Binaries = "", "", ""
		if json.Unmarshal(v.Rule, &rule) == nil {
			endpoints := []string{}
			binaries := []string{}
			for _, endpoint := range rule.Endpoints {
				ports := endpoint.Ports
				if len(ports) == 0 && endpoint.Port != 0 {
					ports = []uint32{endpoint.Port}
				}
				if len(ports) == 0 {
					endpoints = append(endpoints, endpoint.Host)
				}
				for _, port := range ports {
					endpoints = append(endpoints, fmt.Sprintf("%s:%d", endpoint.Host, port))
				}
			}
			for _, binary := range rule.Binaries {
				binaries = append(binaries, binary.Path)
			}
			old.Endpoints = strings.Join(endpoints, ", ")
			old.Binaries = strings.Join(binaries, ", ")
			old.Binary = old.Binaries
		}
	}
	if d := state.Decisions[r.Winner]; d != nil {
		old.Decision, old.DecisionReason = d.Input.Decision, d.Input.Reason
		old.DecisionError = ""
		old.AgentNotificationError = ""
		if d.Receipt.Decision.Error != nil {
			old.DecisionError = d.Receipt.Decision.Error.Error()
		}
		if d.Receipt.Decision.State == "confirmed" {
			old.DecidedAt = d.Receipt.Decision.UpdatedAt
		}
		if d.Receipt.Notification.Status != "delivered" && d.Receipt.Notification.Status != "not_requested" {
			old.AgentNotificationError = "notification " + d.Receipt.Notification.Status
		}
	}
	return old
}

// Called with the store lock held. New records are projections from the one
// authoritative ledger; bounded historical files are only a compatibility view.
func (s *Store) mergeModernAccess(old []AccessRequest, taskID, status string) ([]AccessRequest, error) {
	byID := map[string]AccessRequest{}
	for _, r := range old {
		byID[r.ID] = r
	}
	files, err := filepath.Glob(filepath.Join(s.Home, "access-v1", "*.json"))
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		name := filepath.Base(file)
		id := strings.TrimSuffix(name, ".json")
		if !validID(id) || (taskID != "" && taskID != id) {
			continue
		}
		state, err := s.readAccessState(id)
		if err != nil {
			return nil, err
		}
		if err = s.refreshAccessReceiptViews(&state); err != nil {
			return nil, err
		}
		for rid, r := range state.Requests {
			previous := byID[rid]
			if previous.ID != "" && (previous.TaskID != id || previous.Sandbox != r.View.Target.SandboxName) {
				return nil, fmt.Errorf("access ID belongs to conflicting task or sandbox identities")
			}
			byID[rid] = legacyAccessView(state, r, previous)
		}
	}
	return filterLegacyAccess(byID, status), nil
}
func filterLegacyAccess(byID map[string]AccessRequest, status string) []AccessRequest {
	out := []AccessRequest{}
	for _, r := range byID {
		if status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	return out
}
