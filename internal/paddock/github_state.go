package paddock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type GitHubWatch struct {
	Repo     string           `json:"repo"`
	Number   int              `json:"number"`
	TaskID   string           `json:"task_id"`
	HeadRepo string           `json:"head_repo"`
	HeadRef  string           `json:"head_ref"`
	Since    time.Time        `json:"since"`
	Authors  map[string]int64 `json:"authors"`
}

func (w GitHubWatch) Key() string { return fmt.Sprintf("%s#%d", w.Repo, w.Number) }

type GitHubEvent struct {
	ID        string        `json:"id"`
	WatchKey  string        `json:"watch_key"`
	TaskID    string        `json:"task_id"`
	Comment   GitHubComment `json:"comment"`
	Plan      Plan          `json:"plan"`
	Status    string        `json:"status"`
	Result    string        `json:"result,omitempty"`
	Error     string        `json:"error,omitempty"`
	UpdatedAt string        `json:"updated_at"`
}

type GitHubPost struct {
	ID      string `json:"id"`
	EventID string `json:"event_id"`
	Body    string `json:"body"`
	Status  string `json:"status"`
	URL     string `json:"url,omitempty"`
	Error   string `json:"error,omitempty"`
}

type GitHubState struct {
	Version  int                    `json:"version"`
	Identity GitHubIdentity         `json:"identity"`
	Watches  map[string]GitHubWatch `json:"watches"`
	Events   map[string]GitHubEvent `json:"events"`
	Posts    map[string]GitHubPost  `json:"posts"`
}

func githubID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}
func githubEventID(w GitHubWatch, c GitHubComment) string {
	return githubID(fmt.Sprintf("%s/%s/%d", w.Key(), c.Kind, c.ID))
}

func (s *Store) readGitHub() (GitHubState, error) {
	var v GitHubState
	b, err := os.ReadFile(filepath.Join(s.Home, "github.json"))
	if errors.Is(err, os.ErrNotExist) {
		return GitHubState{Version: 1, Watches: map[string]GitHubWatch{}, Events: map[string]GitHubEvent{}, Posts: map[string]GitHubPost{}}, nil
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("invalid github.json: %w", err)
	}
	if v.Version != 1 || v.Watches == nil || v.Events == nil || v.Posts == nil {
		return v, fmt.Errorf("unsupported or invalid github.json")
	}
	return v, nil
}
func (s *Store) GitHubState() (GitHubState, error) {
	var v GitHubState
	err := s.withLock(func() error { var err error; v, err = s.readGitHub(); return err })
	return v, err
}
func (s *Store) UpdateGitHub(fn func(*GitHubState) error) error {
	return s.withLock(func() error {
		v, err := s.readGitHub()
		if err != nil {
			return err
		}
		if err = fn(&v); err != nil {
			return err
		}
		return writeJSONAtomic(filepath.Join(s.Home, "github.json"), v)
	})
}

// Claim before any external side effect. A crash leaves an inspectable uncertain
// action, never an automatically replayed message or delegation.
func (s *Store) claimGitHub(event GitHubEvent) (bool, error) {
	claimed := false
	err := s.UpdateGitHub(func(v *GitHubState) error {
		if _, exists := v.Events[event.ID]; exists {
			return nil
		}
		w, exists := v.Watches[event.WatchKey]
		if !exists || w.TaskID != event.TaskID {
			return fmt.Errorf("PR watch changed before action")
		}
		if event.Plan.Action != "ignore" && !authorizedGitHub(w, event.Comment) {
			return fmt.Errorf("author authorization changed before action")
		}
		event.Status = "claimed"
		event.UpdatedAt = now()
		v.Events[event.ID] = event
		claimed = true
		return nil
	})
	return claimed, err
}
func authorizedGitHub(w GitHubWatch, c GitHubComment) bool {
	if c.User.Type != "User" || c.User.ID < 1 {
		return false
	}
	for _, id := range w.Authors {
		if id == c.User.ID {
			return true
		}
	}
	return false
}
