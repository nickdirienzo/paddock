package paddock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// All read/modify/write operations hold an OS lock. Hand edits require quiescence.
type Store struct {
	Home string
	lock *os.File
	mu   sync.Mutex
}
type record struct {
	Version int `json:"version"`
	Task
}

func OpenStore(home string) (*Store, error) {
	home, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(home, "tasks"), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return &Store{Home: home, lock: f}, nil
}
func (s *Store) Close() error { return s.lock.Close() }
func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, err := tryLock(s.lock)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task registry is busy; retry shortly")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer unlock(s.lock)
	return fn()
}
func validID(id string) bool {
	if len(id) != 24 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
func (s *Store) path(id string) string { return filepath.Join(s.Home, "tasks", id+".json") }
func (s *Store) read(id string) (Task, error) {
	var r record
	if !validID(id) {
		return r.Task, fmt.Errorf("invalid task ID %q", id)
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return r.Task, err
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return r.Task, fmt.Errorf("read task %s: %w", id, err)
	}
	if r.Version != 1 {
		return r.Task, fmt.Errorf("task %s has unsupported format version %d", id, r.Version)
	}
	if r.ID != id {
		return r.Task, fmt.Errorf("task %s contains a mismatched ID", id)
	}
	return r.Task, nil
}
func (s *Store) write(t Task) error {
	if !validID(t.ID) {
		return fmt.Errorf("invalid task ID %q", t.ID)
	}
	return writeJSONAtomic(s.path(t.ID), record{Version: 1, Task: t})
}

func writeJSONAtomic(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".paddock-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = replaceFile(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func (s *Store) Insert(t Task) error {
	return s.withLock(func() error {
		if !validID(t.ID) {
			return fmt.Errorf("invalid task ID %q", t.ID)
		}
		if _, err := os.Stat(s.path(t.ID)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("task already exists or cannot be accessed: %s", t.ID)
		}
		if t.ParentID != "" {
			if _, err := s.read(t.ParentID); err != nil {
				return fmt.Errorf("parent task: %w", err)
			}
		}
		return s.write(t)
	})
}
func (s *Store) get(prefix string) (Task, error) {
	if prefix == "" {
		return Task{}, fmt.Errorf("task ID is required")
	}
	entries, err := os.ReadDir(filepath.Join(s.Home, "tasks"))
	if err != nil {
		return Task{}, err
	}
	id := ""
	for _, e := range entries {
		candidate := strings.TrimSuffix(e.Name(), ".json")
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && strings.HasPrefix(candidate, prefix) {
			if id != "" {
				return Task{}, fmt.Errorf("task prefix %q is ambiguous", prefix)
			}
			id = candidate
		}
	}
	if id == "" {
		return Task{}, fmt.Errorf("task %q not found", prefix)
	}
	return s.read(id)
}
func (s *Store) Get(id string) (Task, error) {
	var t Task
	err := s.withLock(func() error { var err error; t, err = s.get(id); return err })
	return t, err
}
func (s *Store) List(parent string) ([]Task, error) {
	out := []Task{}
	err := s.withLock(func() error {
		entries, err := os.ReadDir(filepath.Join(s.Home, "tasks"))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			t, err := s.read(strings.TrimSuffix(e.Name(), ".json"))
			if err != nil {
				return err
			}
			if parent == "" || t.ParentID == parent {
				out = append(out, t)
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, err
}
func (s *Store) update(id string, fn func(*Task) error) error {
	return s.withLock(func() error {
		t, err := s.read(id)
		if err != nil {
			return err
		}
		if err = fn(&t); err != nil {
			return err
		}
		t.UpdatedAt = now()
		return s.write(t)
	})
}
func (s *Store) UpdateSession(id string, fn func(*Session)) error {
	return s.update(id, func(t *Task) error { fn(&t.Session); return nil })
}
func (s *Store) Status(id, status, message string, from ...string) error {
	return s.update(id, func(t *Task) error {
		allowed := len(from) == 0
		for _, v := range from {
			if t.Status == v {
				allowed = true
			}
		}
		if allowed {
			t.Status = status
			t.Error = message
		}
		return nil
	})
}
func (s *Store) Claim(id string) error {
	return s.claimGeneration(id, nil)
}

func (s *Store) claimGeneration(id string, generation *int) error {
	return s.update(id, func(t *Task) error {
		if generation != nil && t.Session.Generation != *generation {
			return fmt.Errorf("stale supervisor generation")
		}
		if t.Status != "starting" {
			return fmt.Errorf("task %s cannot start from %s", id, t.Status)
		}
		t.Status = "running"
		t.Session.StartedAt = now()
		t.Session.ResumeNotice = ""
		t.LastActivityAt = now()
		return nil
	})
}
func (s *Store) Finish(id string, code int, message string) error {
	return s.update(id, func(t *Task) error {
		t.Session.ExitCode = &code
		t.Session.EndedAt = now()
		t.LastActivityAt = now()
		t.Error = message
		if code == 0 {
			t.Status = "stopped"
		} else {
			t.Status = "failed"
		}
		return nil
	})
}
