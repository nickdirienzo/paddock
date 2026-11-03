package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func nativeMessageSupported(t Task) bool {
	return t.Session.Agent == "codex" || (t.Session.Agent == "claude" && t.RuntimeName() == "openshell")
}

// The pinned Claude inbox is a peer-message transport, never terminal input.
// Its native admission policy and tool approvals remain authoritative.
func (s Service) messageClaude(ctx context.Context, t Task, body string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if t.Session.ConversationID == "" {
		return "", fmt.Errorf("%w: no saved Claude conversation", ErrAgentOffline)
	}
	if err := s.Store.TouchActivity(t.ID); err != nil {
		return "", err
	}
	command := sandboxExec(ctx, t, false, "/usr/local/bin/paddock-claude-message", t.Session.ConversationID, fmt.Sprint(t.Session.Generation))
	payload, _ := json.Marshal(map[string]string{"message": body})
	command.Stdin = strings.NewReader(string(payload))
	output, err := command.Output()
	if err != nil {
		return t.Session.ConversationID, fmt.Errorf("Claude delivery outcome uncertain: %w; inspect the session before retrying", err)
	}
	switch strings.TrimSpace(string(output)) {
	case "offline":
		return "", fmt.Errorf("%w: Claude inbox is not ready", ErrAgentOffline)
	case "queued":
		return t.Session.ConversationID, nil
	default:
		return t.Session.ConversationID, fmt.Errorf("Claude inbox delivery unconfirmed; the message may be held, refused, or queued; inspect the session before retrying")
	}
}
