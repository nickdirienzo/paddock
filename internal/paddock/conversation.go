package paddock

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var conversationUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func verifyConversation(ctx context.Context, t Task) error {
	if !conversationUUID.MatchString(t.Session.ConversationID) {
		return fmt.Errorf("missing or invalid native conversation ID")
	}
	switch t.Session.Agent {
	case "codex":
		_, err := codexTaskThread(ctx, t)
		return err
	case "claude":
		// Claude's persisted native project transcripts live outside the worktree.
		// Never use --continue or --fork-session, which can select a different ID.
		paths, err := filepath.Glob(filepath.Join(t.Session.AgentHome, "projects", "*", t.Session.ConversationID+".jsonl"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				return nil
			}
		}
		return fmt.Errorf("Claude conversation history is missing; refusing to start a replacement conversation")
	default:
		return fmt.Errorf("unsupported agent %s", t.Session.Agent)
	}
}

// Capture identity early, without waiting for the first GitHub message. This is
// metadata discovery, not an idle/approval detector or authorization to kill.
func captureConversation(ctx context.Context, store *Store, t Task) {
	if t.Session.Generation == 0 || t.Session.Agent != "codex" || t.Session.ConversationID != "" {
		return
	}
	versionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	cmd := exec.CommandContext(versionCtx, t.Session.AgentPath, "--version")
	cmd.Dir, cmd.Env = t.Worktree, agentControlEnv(t)
	b, err := cmd.Output()
	cancel()
	if err != nil || !strings.HasPrefix(string(b), "codex-cli ") {
		return
	}
	for ctx.Err() == nil {
		id, err := codexTaskThread(ctx, t)
		if err == nil {
			_ = store.update(t.ID, func(current *Task) error {
				if current.Session.Generation != t.Session.Generation {
					return fmt.Errorf("stale conversation discovery")
				}
				if current.Session.ConversationID != "" && current.Session.ConversationID != id {
					return fmt.Errorf("conversation identity changed")
				}
				current.Session.ConversationID = id
				return nil
			})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}
