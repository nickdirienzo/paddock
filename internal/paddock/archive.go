package paddock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConversationArchive stores readable chat for a new session, not a native backup.
// No credentials, provider configuration, or agent-home directories are copied.
type ConversationArchive struct {
	Generation             int    `json:"generation"`
	ShippedCommit          string `json:"shipped_commit,omitempty"`
	ContextText            string `json:"context_text,omitempty"`
	ContextSHA256          string `json:"context_sha256,omitempty"`
	Version                int    `json:"version"`
	TaskID                 string `json:"task_id"`
	ConversationID         string `json:"conversation_id"`
	Agent                  string `json:"agent"`
	Runtime                string `json:"runtime"`
	Worktree               string `json:"worktree"`
	CreatedAt              string `json:"created_at"`
	SHA256                 string `json:"sha256"`
	Transcript             []byte `json:"transcript,omitempty"`
	NativeRestoreQualified bool   `json:"native_restore_qualified"`
}

// A single atomic envelope binds the manifest and readable chat bytes. An
// interrupted write cannot publish a manifest pointing at incomplete data.
func (s Service) ArchiveConversation(ctx context.Context, id string) (ConversationArchive, error) {
	t, err := s.Store.Get(id)
	if err != nil {
		return ConversationArchive{}, err
	}
	release, err := s.Store.taskLease(t.ID, "operation")
	if err != nil {
		return ConversationArchive{}, err
	}
	defer release()
	t, err = s.Store.Get(t.ID)
	if err != nil {
		return ConversationArchive{}, err
	}
	releaseProcess, err := s.Store.taskLease(t.ID, "process")
	if err != nil {
		return ConversationArchive{}, err
	}
	defer releaseProcess()
	if t.Status == "running" || t.Status == "starting" || t.Status == "preparing" {
		return ConversationArchive{}, fmt.Errorf("task may still be running")
	}
	_, obs := terminalFor(t.Session).Inspect(ctx, t)
	if obs.State != "missing" && obs.State != "exited" {
		return ConversationArchive{}, fmt.Errorf("terminal process is active or unknown")
	}
	if t.RuntimeName() == "openshell" {
		if t.OpenShell == nil {
			return ConversationArchive{}, fmt.Errorf("missing pinned sandbox")
		}
		state, e := inspectTaskSandbox(ctx, t)
		if e != nil {
			return ConversationArchive{}, e
		}
		if state.Phase != "Stopped" {
			return ConversationArchive{}, fmt.Errorf("sandbox must be confirmed stopped before archival")
		}
		if _, e = t.OpenShell.Config.call(ctx, "sandbox", "start", t.OpenShell.Name); e != nil {
			return ConversationArchive{}, e
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			_, _ = t.OpenShell.Config.call(stopCtx, "sandbox", "stop", t.OpenShell.Name)
		}()
	}
	return s.archiveConversation(ctx, t)
}

func (s Service) archiveConversation(ctx context.Context, t Task) (ConversationArchive, error) {
	var a ConversationArchive
	if !validID(t.ID) || !conversationUUID.MatchString(t.Session.ConversationID) {
		return a, fmt.Errorf("invalid task or conversation identity")
	}
	// Restrict discovery to one exact native transcript, never a home backup.
	pattern := ""
	switch t.Session.Agent {
	case "claude":
		pattern = filepath.Join(t.Session.AgentHome, "projects", "*", t.Session.ConversationID+".jsonl")
	case "codex":
		pattern = filepath.Join(t.Session.AgentHome, "sessions", "*", "*", "*", "rollout-*-"+t.Session.ConversationID+".jsonl")
	default:
		return a, fmt.Errorf("unsupported agent")
	}
	var b []byte
	var err error
	if t.RuntimeName() == "openshell" {
		// The sandbox must already be available for maintenance. No start/delete here.
		out, e := sandboxOutput(ctx, t, "python3", "-c", archiveReadScript, pattern)
		err = e
		if err == nil {
			err = json.Unmarshal([]byte(out), &b)
		}
	} else {
		var paths []string
		paths, err = filepath.Glob(pattern)
		if err == nil && len(paths) != 1 {
			err = fmt.Errorf("native transcript missing or ambiguous")
		}
		if err == nil {
			b, err = readArchiveRegular(paths[0])
		}
	}
	if err != nil {
		return a, err
	}
	if len(b) == 0 || len(b) > 64<<20 {
		return a, fmt.Errorf("empty or oversized transcript")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !json.Valid([]byte(line)) {
			return a, fmt.Errorf("invalid native JSONL transcript")
		}
	}
	sum := sha256.Sum256(b)
	a = ConversationArchive{Generation: t.Session.Generation, ShippedCommit: t.ShippedCommit, Version: 2, TaskID: t.ID, ConversationID: t.Session.ConversationID, Agent: t.Session.Agent, Runtime: t.RuntimeName(), Worktree: t.Worktree, CreatedAt: now(), SHA256: hex.EncodeToString(sum[:]), Transcript: b}
	contextText, contextErr := conversationContext(a)
	if contextErr != nil {
		return a, contextErr
	}
	if t.Session.ContextFile != "" {
		prior, e := s.loadArchivedContext(t)
		if e != nil {
			return a, e
		}
		contextText = prior + "\n\nLATER CONVERSATION IN TASK " + t.ID + "\n" + contextText
	}
	if len(contextText) > maxConversationContextBytes {
		return a, fmt.Errorf("combined conversation exceeds context limit; refusing truncation")
	}
	a.ContextText = contextText
	contextSum := sha256.Sum256([]byte(contextText))
	a.ContextSHA256 = hex.EncodeToString(contextSum[:])
	// Version 2 retains only public chat text. Tool traces, reasoning and native
	// implementation state are not needed for fresh-session context recovery.
	a.Transcript = nil
	a.SHA256 = a.ContextSHA256
	dir := filepath.Join(s.Store.Home, "archives")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return a, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return a, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return a, fmt.Errorf("archive directory must be private and not a symlink")
	}
	if err = syncDirectory(s.Store.Home); err != nil {
		return a, err
	}
	if err = writeJSONAtomic(filepath.Join(dir, t.ID+".json"), a); err != nil {
		return a, err
	}
	return s.VerifyArchive(t.ID)
}

const archiveReadScript = `import base64,glob,json,os,stat,sys
p=glob.glob(sys.argv[1])
assert len(p)==1, 'native transcript missing or ambiguous'
q=os.path.dirname(p[0])
while q!='/':
 assert stat.S_ISDIR(os.lstat(q).st_mode), 'symlink parent'
 q=os.path.dirname(q)
f=os.open(p[0],os.O_RDONLY|os.O_NOFOLLOW)
s=os.fstat(f)
assert stat.S_ISREG(s.st_mode) and 0<s.st_size<=64*1024*1024
with os.fdopen(f,'rb') as r: b=r.read(64*1024*1024+1)
assert len(b)<=64*1024*1024
print(json.dumps(base64.b64encode(b).decode()))`

func readArchiveRegular(path string) ([]byte, error) {
	for parent := filepath.Dir(path); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		info, e := os.Lstat(parent)
		if e != nil {
			return nil, e
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("symlink or non-directory archive parent")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 100<<20 {
		return nil, fmt.Errorf("archive input must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("archive input changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, 100<<20+1))
	if len(b) > 100<<20 {
		return nil, fmt.Errorf("archive input exceeds limit")
	}
	return b, err
}

func (s Service) VerifyArchive(id string) (ConversationArchive, error) {
	var a ConversationArchive
	t, err := s.Store.Get(id)
	if err != nil {
		return a, err
	}
	archivePath := filepath.Join(s.Store.Home, "archives", t.ID+".json")
	if err = privateArchivePath(archivePath); err != nil {
		return a, err
	}
	b, err := readArchiveRegular(archivePath)
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal(b, &a); err != nil {
		return a, err
	}
	if a.Generation != t.Session.Generation || a.ShippedCommit != t.ShippedCommit || (a.Version != 1 && a.Version != 2) || a.TaskID != t.ID || a.ConversationID != t.Session.ConversationID || a.Agent != t.Session.Agent || a.Runtime != t.RuntimeName() || a.Worktree != t.Worktree || a.NativeRestoreQualified {
		return a, fmt.Errorf("archive identity check failed")
	}
	if a.Version == 1 {
		if len(a.Transcript) == 0 || digest(a.Transcript) != a.SHA256 {
			return a, fmt.Errorf("native archive integrity failure")
		}
	} else if a.ContextText == "" || len(a.Transcript) != 0 || digest([]byte(a.ContextText)) != a.ContextSHA256 || a.ContextSHA256 != a.SHA256 {
		return a, fmt.Errorf("readable archive integrity failure")
	}
	return a, nil
}

// MarkShipped is an explicit registry-owner operation, not an agent completion
// signal. Bind approval to HEAD so later commits require a new approval.
func (s Service) MarkShipped(ctx context.Context, id string) error {
	if os.Getenv("PADDOCK_TASK_ID") != "" {
		return fmt.Errorf("shipment confirmation requires the host registry operator")
	}
	t, err := s.Store.Get(id)
	if err != nil {
		return err
	}
	release, err := s.Store.taskLease(t.ID, "operation")
	if err != nil {
		return err
	}
	defer release()
	t, err = s.Store.Get(t.ID)
	if err != nil {
		return err
	}
	head, err := taskGit(ctx, t, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	return s.Store.update(t.ID, func(v *Task) error { v.ShippedCommit = head; v.ShippedAt = now(); return nil })
}
func shipmentGate(ctx context.Context, t Task) error {
	if t.ShippedAt == "" || t.ShippedCommit == "" {
		return fmt.Errorf("shipment not explicitly confirmed by registry operator")
	}
	shipped, err := time.Parse(time.RFC3339Nano, t.ShippedAt)
	if err != nil {
		return err
	}
	if t.LastActivityAt != "" {
		activity, e := time.Parse(time.RFC3339Nano, t.LastActivityAt)
		if e != nil {
			return e
		}
		if activity.After(shipped) {
			return fmt.Errorf("task activity after shipment requires operator reconfirmation")
		}
	}
	head, err := taskGit(ctx, t, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != t.ShippedCommit {
		return fmt.Errorf("workspace changed since shipment confirmation")
	}
	return nil
}
