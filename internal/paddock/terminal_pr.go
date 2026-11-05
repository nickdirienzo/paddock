package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// A merged PR is a terminal label hint, not an agent completion signal. Do not
// refresh task activity or launch a terminal just to display it. GitHub merges
// are permanent, so no separate label ownership or restoration state is needed.
func markRexSessionMerged(ctx context.Context, s Session) error {
	if s.Backend != "rex" || s.ID == "" {
		return nil
	}
	t := terminal{backend: s.Backend, path: s.TerminalPath, endpoint: s.Endpoint}
	b, err := t.rex(ctx, "ls", "--json")
	if err != nil {
		return fmt.Errorf("merged PR label: %w", err)
	}
	var list struct {
		Sessions []struct {
			ID    string `json:"session_id"`
			Label string `json:"label"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("merged PR label: %w", err)
	}
	for _, session := range list.Sessions {
		if session.ID != s.ID {
			continue
		}
		// A section may contain several tasks. Label only the owned tab;
		// legacy records without window metadata retain session labels.
		if s.WindowID != "" {
			return markRexWindowMerged(ctx, t, s)
		}
		const suffix = " ✓ merged"
		if strings.HasSuffix(session.Label, suffix) {
			return nil
		}
		label := session.Label + suffix
		// Preserve the user's full label rather than truncating it to make room.
		if utf8.RuneCountInString(label) > 128 {
			return fmt.Errorf("merged PR label: session label has no room for merge suffix")
		}
		args, err := json.Marshal(map[string]string{"session_id": s.ID, "label": label})
		if err != nil {
			return err
		}
		_, err = t.rex(ctx, "api", "call", "session.set_label", string(args))
		if err != nil {
			return fmt.Errorf("merged PR label: %w", err)
		}
		return nil
	}
	return nil // The session was closed; never recreate it for a label.
}

func markRexWindowMerged(ctx context.Context, t terminal, s Session) error {
	b, err := t.rex(ctx, "session", "inspect", s.ID, "--json")
	if err != nil {
		return fmt.Errorf("merged PR label: %w", err)
	}
	var state struct {
		Windows []struct {
			ID    string `json:"window_id"`
			Label string `json:"label"`
		} `json:"windows"`
	}
	if err = json.Unmarshal(b, &state); err != nil {
		return fmt.Errorf("merged PR label: %w", err)
	}
	for _, window := range state.Windows {
		if window.ID != s.WindowID {
			continue
		}
		const suffix = " ✓ merged"
		if strings.HasSuffix(window.Label, suffix) {
			return nil
		}
		label := window.Label + suffix
		if utf8.RuneCountInString(label) > 128 {
			return fmt.Errorf("merged PR label: window label has no room for merge suffix")
		}
		_, err = t.rex(ctx, "window", "rename", "-s", s.ID, s.WindowID, label)
		if err != nil {
			return fmt.Errorf("merged PR label: %w", err)
		}
		return nil
	}
	return nil
}
