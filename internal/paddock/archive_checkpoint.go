package paddock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func privateArchivePath(path string) error {
	for _, p := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("archive paths must be private and cannot be symlinks")
		}
	}
	return nil
}

// An immutable checkpoint survives a later session replacing the task's latest
// native archive. The task record points to its exact complete-envelope digest.
func (s Service) checkpointConversation(ctx context.Context, t Task) (TaskArchiveState, error) {
	var state TaskArchiveState
	a, err := s.archiveConversation(ctx, t)
	if err != nil {
		return state, err
	}
	if a.ContextText == "" {
		return state, fmt.Errorf("archive lacks readable conversation context")
	}
	bytes, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return state, err
	}
	bytes = append(bytes, '\n')
	hash := digest(bytes)
	filename := fmt.Sprintf("%s-%d-%s.json", t.ID, t.Session.Generation, hash)
	path := filepath.Join(s.Store.Home, "archives", filename)
	// Content addressing makes retries idempotent and never overwrites a different
	// generation/checkpoint. Existing bytes must match their name and digest.
	if old, e := readArchiveRegular(path); e == nil {
		if digest(old) != hash {
			return state, fmt.Errorf("immutable archive checkpoint mismatch")
		}
	} else if os.IsNotExist(e) {
		if err = writeJSONAtomic(path, a); err != nil {
			return state, err
		}
	} else {
		return state, e
	}
	state = TaskArchiveState{Phase: "deleting", File: filename, SHA256: hash, SourceConversationID: t.Session.ConversationID, SourceGeneration: t.Session.Generation, PreviousBranch: t.Branch, PreviousWorktree: t.Worktree, CapturedAt: a.CreatedAt, ContextSHA256: a.ContextSHA256}
	probe := t
	probe.Archive = &state
	if _, err = s.loadArchivedContext(probe); err != nil {
		return state, err
	}
	return state, nil
}

func (s Service) loadArchivedContext(t Task) (string, error) {
	if t.Archive == nil {
		return "", fmt.Errorf("task has no archive checkpoint")
	}
	ref := t.Archive
	if !validID(t.ID) || ref.File != filepath.Base(ref.File) || !strings.HasPrefix(ref.File, t.ID+"-") || !strings.HasSuffix(ref.File, ".json") || len(ref.SHA256) != 64 {
		return "", fmt.Errorf("invalid task archive reference")
	}
	path := filepath.Join(s.Store.Home, "archives", ref.File)
	if err := privateArchivePath(path); err != nil {
		return "", err
	}
	bytes, err := readArchiveRegular(path)
	if err != nil {
		return "", err
	}
	if digest(bytes) != ref.SHA256 {
		return "", fmt.Errorf("archive checkpoint integrity failure")
	}
	var a ConversationArchive
	if err = json.Unmarshal(bytes, &a); err != nil {
		return "", err
	}
	if a.Version != 2 || a.NativeRestoreQualified || len(a.ContextText) > maxConversationContextBytes || a.TaskID != t.ID || a.Agent != t.Session.Agent || a.Runtime != t.RuntimeName() || a.ConversationID != ref.SourceConversationID || a.Generation != ref.SourceGeneration || a.Worktree != ref.PreviousWorktree || a.ContextText == "" || a.ContextSHA256 != ref.ContextSHA256 || digest([]byte(a.ContextText)) != ref.ContextSHA256 || len(a.Transcript) != 0 || a.ContextSHA256 != a.SHA256 {
		return "", fmt.Errorf("task archive checkpoint identity or integrity mismatch")
	}
	return a.ContextText, nil
}

// ArchivedContext is task-ID discovery for Team Principal. It verifies the
// immutable checkpoint even after a new native conversation changes identity.
func (s Service) ArchivedContext(id string) (string, error) {
	t, err := s.Store.Get(id)
	if err != nil {
		return "", err
	}
	if t.Archive != nil {
		return s.loadArchivedContext(t)
	}
	a, err := s.VerifyArchive(t.ID)
	if err != nil {
		return "", err
	}
	if a.Version == 2 {
		return a.ContextText, nil
	}
	return conversationContext(a)
}
