package paddock

import (
	"context"
	"encoding/json"
	"fmt"
)

// Persist the destination before launch so a lost window-creation reply can be
// reconciled by its deterministic label without adopting a sibling's terminal.
func (s Service) prepareRexGroup(ctx context.Context, task *Task) error {
	if task.Session.Backend != "rex" || (task.ParentID == "" && task.Session.GroupID == "") {
		return nil
	}
	var candidates []Session
	seen := map[string]bool{task.ID: true}
	for id := task.ParentID; id != ""; {
		if seen[id] {
			return fmt.Errorf("cycle in task parentage")
		}
		seen[id] = true
		parent, err := s.Store.Get(id)
		if err != nil {
			return err
		}
		if parent.Session.Backend == "rex" && parent.Session.Endpoint == task.Session.Endpoint {
			candidates = append(candidates, parent.Session)
		}
		id = parent.ParentID
	}
	if task.Session.GroupID != "" {
		candidates = append(candidates, Session{ID: task.Session.GroupID})
	}
	group := ""
	if len(candidates) > 0 {
		term := terminal{path: task.Session.TerminalPath, endpoint: task.Session.Endpoint}
		b, err := term.rex(ctx, "ls", "--json")
		if err != nil {
			return err
		}
		var list struct {
			Sessions []struct {
				ID    string `json:"session_id"`
				Label string `json:"label"`
			} `json:"sessions"`
		}
		if err = json.Unmarshal(b, &list); err != nil {
			return err
		}
		if list.Sessions == nil {
			return fmt.Errorf("Rex returned incomplete session listing")
		}
		for _, candidate := range candidates {
			for _, session := range list.Sessions {
				if session.ID != "" && (session.ID == candidate.ID || session.ID == candidate.GroupID ||
					(candidate.ID == "" && candidate.Name != "" && session.Label == candidate.Name)) {
					group = session.ID
					break
				}
			}
			if group != "" {
				break
			}
		}
	}
	if err := s.Store.UpdateSession(task.ID, func(session *Session) { session.GroupID = group }); err != nil {
		return err
	}
	task.Session.GroupID = group
	return nil
}
