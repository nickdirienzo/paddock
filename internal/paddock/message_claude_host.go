package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const claudeInboxVersion = "2.1.325 (Claude Code)"

func qualifiedClaudeInboxVersion(version string) bool {
	return version == "2.1.286 (Claude Code)" || version == claudeInboxVersion
}

type claudeInbox struct {
	PID          int    `json:"pid"`
	Conversation string `json:"conversation"`
	Generation   int    `json:"generation"`
	Socket       string `json:"socket"`
}

func (s *Store) claudeInboxFile(id string) string {
	return filepath.Join(s.Home, "tasks", id+".claude-inbox")
}

// Only the supervisor holding the process lease creates this private inbox.
// A short temporary path also fits macOS's Unix socket path limit.
func prepareHostClaudeInbox(t Task, c *exec.Cmd) (claudeInbox, func(), error) {
	var inbox claudeInbox
	noop := func() {}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version, err := agentCommandContext(ctx, t, "--version").Output()
	if err != nil || !qualifiedClaudeInboxVersion(strings.TrimSpace(string(version))) {
		return inbox, noop, fmt.Errorf("Claude inbox delivery requires qualified Claude Code 2.1.286 or 2.1.325 (version check: %q, %v)", strings.TrimSpace(string(version)), err)
	}
	dir, err := os.MkdirTemp("", "pd-")
	if err != nil {
		return inbox, noop, err
	}
	inbox = claudeInbox{Conversation: t.Session.ConversationID, Generation: t.Session.Generation, Socket: filepath.Join(dir, "inbox")}
	c.Args = append([]string{c.Args[0], "--messaging-socket-path", inbox.Socket}, c.Args[1:]...)
	return inbox, func() { _ = os.RemoveAll(dir) }, nil
}

func (s Service) messageHostClaude(ctx context.Context, t Task, body string) (string, error) {
	inbox, err := s.Store.hostClaudeInbox(t)
	if err != nil {
		return "", fmt.Errorf("%w: Claude inbox is not ready; host sessions launched before inbox support must exit and be resumed: %v", ErrAgentOffline, err)
	}
	if err = s.Store.TouchActivity(t.ID); err != nil {
		return "", err
	}
	return deliverHostClaude(ctx, t, inbox, body)
}

func (s *Store) hostClaudeInbox(t Task) (claudeInbox, error) {
	data, err := os.ReadFile(s.claudeInboxFile(t.ID))
	var inbox claudeInbox
	if err != nil || json.Unmarshal(data, &inbox) != nil || inbox.PID <= 0 ||
		inbox.Conversation != t.Session.ConversationID || inbox.Generation != t.Session.Generation {
		return inbox, fmt.Errorf("missing or stale Claude inbox metadata")
	}
	info, err := os.Lstat(inbox.Socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return inbox, fmt.Errorf("Claude inbox socket is unavailable")
	}
	return inbox, nil
}

// Before the first write, failures are safely retryable. After any write, only
// an exact native enqueue record confirms admission; failures stay uncertain.
func deliverHostClaude(ctx context.Context, t Task, inbox claudeInbox, body string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	offline := func(err error) (string, error) {
		return "", fmt.Errorf("%w: Claude inbox is not ready: %v", ErrAgentOffline, err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", inbox.Socket)
	if err != nil {
		return offline(err)
	}
	defer conn.Close()
	if err = verifyClaudePeer(conn.(*net.UnixConn), inbox.PID); err != nil {
		return offline(err)
	}
	paths, err := filepath.Glob(filepath.Join(t.Session.AgentHome, "projects", "*", t.Session.ConversationID+".jsonl"))
	if err != nil || len(paths) != 1 {
		return offline(fmt.Errorf("expected one native conversation history"))
	}
	log, err := os.Open(paths[0])
	if err != nil {
		return offline(err)
	}
	defer log.Close()
	if info, err := log.Stat(); err != nil || !info.Mode().IsRegular() {
		return offline(fmt.Errorf("native history is not a regular file"))
	}
	if _, err = log.Seek(0, io.SeekEnd); err != nil {
		return offline(err)
	}
	content := "Paddock delivery " + newConversationID() + "\n" + body
	frame := map[string]any{
		"type": "user", "uuid": newConversationID(), "msg_id": newConversationID(),
		"from": "paddock", "priority": "next",
		"message": map[string]string{"role": "user", "content": content},
	}
	deadline, _ := ctx.Deadline()
	if err = conn.SetWriteDeadline(deadline); err != nil {
		return offline(err)
	}
	uncertain := func(err error) (string, error) {
		return t.Session.ConversationID, fmt.Errorf("Claude inbox delivery unconfirmed; the message may be held, refused, or queued; inspect the session before retrying: %v", err)
	}
	if err = json.NewEncoder(conn).Encode(frame); err != nil {
		return uncertain(err)
	}
	if err = conn.(*net.UnixConn).CloseWrite(); err != nil {
		return uncertain(err)
	}
	reader := bufio.NewReader(log)
	pending := ""
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return uncertain(ctx.Err())
		}
		line, err := reader.ReadString('\n')
		pending += line
		if strings.HasSuffix(pending, "\n") {
			var event struct {
				Type      string `json:"type"`
				Operation string `json:"operation"`
				SessionID string `json:"sessionId"`
				Content   string `json:"content"`
			}
			if json.Unmarshal([]byte(pending), &event) == nil && event.Type == "queue-operation" &&
				event.Operation == "enqueue" && event.SessionID == t.Session.ConversationID && event.Content == content {
				return t.Session.ConversationID, nil
			}
			pending = ""
		}
		if err == nil {
			continue
		}
		if err != io.EOF {
			return uncertain(err)
		}
		select {
		case <-ctx.Done():
			return uncertain(ctx.Err())
		case <-ticker.C:
		}
	}
}
