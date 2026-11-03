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
	PlannerModel  string              `json:"planner_model,omitempty"`
	Routing       string              `json:"routing,omitempty"`
	Orchestration OrchestrationPolicy `json:"orchestration,omitzero"`
	Repo          string              `json:"repo"`
	Number        int                 `json:"number"`
	TaskID        string              `json:"task_id"`
	HeadRepo      string              `json:"head_repo"`
	HeadRef       string              `json:"head_ref"`
	Since         time.Time           `json:"since"`
	Authors       map[string]int64    `json:"authors"`
}

func (w GitHubWatch) Key() string { return fmt.Sprintf("%s#%d", w.Repo, w.Number) }

type GitHubEvent struct {
	SignalKey        string                `json:"signal_key,omitempty"`
	RecipientTaskIDs []string              `json:"recipient_task_ids,omitempty"`
	Identity         GitHubIdentityBinding `json:"identity,omitzero"`
	ID               string                `json:"id"`
	WatchKey         string                `json:"watch_key"`
	TaskID           string                `json:"task_id"`
	Comment          GitHubComment         `json:"comment"`
	Plan             Plan                  `json:"plan"`
	Status           string                `json:"status"`
	Result           string                `json:"result,omitempty"`
	Error            string                `json:"error,omitempty"`
	Receipt          string                `json:"receipt,omitempty"`
	ReceiptError     string                `json:"receipt_error,omitempty"`
	UpdatedAt        string                `json:"updated_at"`
}

type GitHubPost struct {
	Identity GitHubIdentityBinding `json:"identity,omitzero"`
	ID       string                `json:"id"`
	EventID  string                `json:"event_id"`
	Body     string                `json:"body"`
	Status   string                `json:"status"`
	URL      string                `json:"url,omitempty"`
	Error    string                `json:"error,omitempty"`
}

type GitHubState struct {
	Version    int                       `json:"version"`
	Identity   GitHubIdentity            `json:"identity,omitzero"`
	Identities map[string]GitHubIdentity `json:"identities"`
	Routes     map[string]string         `json:"routes"`
	Watches    map[string]GitHubWatch    `json:"watches"`
	Events     map[string]GitHubEvent    `json:"events"`
	Posts      map[string]GitHubPost     `json:"posts"`
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
		return GitHubState{Version: 1, Identities: map[string]GitHubIdentity{}, Routes: map[string]string{}, Watches: map[string]GitHubWatch{}, Events: map[string]GitHubEvent{}, Posts: map[string]GitHubPost{}}, nil
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
	if v.Identities == nil {
		v.Identities = map[string]GitHubIdentity{}
	}
	if v.Routes == nil {
		v.Routes = map[string]string{}
	}
	if v.Identity.Mode != "" {
		v.Identities["default"] = v.Identity
		binding := bindGitHubIdentity("default", v.Identity)
		for id, post := range v.Posts {
			if post.Identity.Name == "" {
				post.Identity = binding
				v.Posts[id] = post
			}
		}
		for id, event := range v.Events {
			if event.Identity.Name == "" {
				event.Identity = binding
				v.Events[id] = event
			}
		}
		if _, ok := v.Routes["*"]; !ok {
			v.Routes["*"] = "default"
		}
		v.Identity = GitHubIdentity{}
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
