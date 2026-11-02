package paddock

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"
)

// Task owns its worktree. Agent and terminal details belong to its Session.
type Task struct {
	ID         string  `json:"id"`
	ParentID   string  `json:"parent_id,omitempty"`
	Prompt     string  `json:"prompt"`
	Repo       string  `json:"repo"`
	CommonDir  string  `json:"common_dir"`
	Base       string  `json:"base"`
	BaseCommit string  `json:"base_commit"`
	Branch     string  `json:"branch"`
	Worktree   string  `json:"worktree"`
	Status     string  `json:"status"`
	Error      string  `json:"error,omitempty"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	Session    Session `json:"session"`
}

type Session struct {
	ConversationID string `json:"conversation_id,omitempty"`
	AgentHome      string `json:"agent_home,omitempty"`
	Agent          string `json:"agent"`
	TrustProject   bool   `json:"trust_project,omitempty"`
	AgentPath      string `json:"agent_path"`
	Backend        string `json:"backend"`
	TerminalPath   string `json:"terminal_path"`
	Endpoint       string `json:"endpoint,omitempty"`
	Name           string `json:"name"`
	ID             string `json:"id,omitempty"`
	BlockID        string `json:"block_id,omitempty"`
	WindowID       string `json:"window_id,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	EndedAt        string `json:"ended_at,omitempty"`
}

type Observation struct {
	State string `json:"state"` // running, exited, missing, unknown
	Error string `json:"error,omitempty"`
}

type Detail struct {
	Task
	Terminal Observation `json:"terminal"`
	Children []Task      `json:"children"`
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func StateHome() (string, error) {
	if p := os.Getenv("PADDOCK_HOME"); p != "" {
		return filepath.Abs(p)
	}
	if p := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(p) {
		return filepath.Join(p, "paddock"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "paddock"), nil
}
