package paddock

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

//go:embed sandbox_shell.py
var sandboxShellClient string

// OpenHostShell opens a new shell tab without replacing the running agent.
// A nil task means an ordinary host invocation; sandbox requests always supply
// their host-owned task record, never a directory or command from the sandbox.
func (s Service) OpenHostShell(ctx context.Context, task *Task, client string) (Session, error) {
	var term terminal
	session, block := os.Getenv("REX_SESSION"), os.Getenv("REX_BLOCK")
	dir, err := os.Getwd()
	if err != nil {
		return Session{}, err
	}
	var command []string
	if task != nil {
		if task.Session.Backend != "rex" {
			return Session{}, fmt.Errorf("host shell tabs require Rex")
		}
		term = terminal{backend: "rex", path: task.Session.TerminalPath, endpoint: task.Session.Endpoint}
		session, block = task.Session.ID, task.Session.BlockID
		dir, err = hostSandboxWorktree(ctx, *task)
		if err != nil {
			return Session{}, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return Session{}, err
		}
		// This is a host inspection shell. Keep sandbox project configuration
		// out of host mise activation without changing any trust records.
		ignoredConfigs := filepath.Join(home, "OrbStack", "docker", "containers")
		if prior := os.Getenv("MISE_IGNORED_CONFIG_PATHS"); prior != "" {
			ignoredConfigs = prior + string(os.PathListSeparator) + ignoredConfigs
		}
		shell := os.Getenv("SHELL")
		if !filepath.IsAbs(shell) {
			shell = "/bin/sh"
		}
		command = []string{"--", "/usr/bin/env", "PADDOCK_HOME=" + s.Store.Home, "PADDOCK_TASK_ID=" + task.ID,
			"MISE_IGNORED_CONFIG_PATHS=" + ignoredConfigs,
			"PATH=" + filepath.Dir(s.Executable) + string(os.PathListSeparator) + os.Getenv("PATH"),
			"/bin/sh", "-c", `printf '\nHost shell for Paddock task %s.\nThis directory is the live sandbox worktree.\nInspect files directly with open, Quick Look, or other Mac tools.\n\n' "$PADDOCK_TASK_ID"; exec "$@"`, "paddock-host-shell", shell, "-l"}
	} else {
		t, _, e := SelectTerminal(ctx, "rex")
		if e != nil {
			return Session{}, e
		}
		term = t.(terminal)
		if client == "" {
			client = os.Getenv("REX_CLIENT")
		}
	}
	client, err = term.rexClient(ctx, client, session, block)
	if err != nil {
		return Session{}, err
	}
	// The inherited session may have disappeared since this terminal started.
	if session != "" {
		b, e := term.rex(ctx, "session", "inspect", session, "--json")
		var v struct {
			ID string `json:"session_id"`
		}
		if e != nil || json.Unmarshal(b, &v) != nil || v.ID != session {
			session = ""
		}
	}
	args := []string{"window", "new", "-s", session, "Host Shell", "-c", dir, "--json"}
	if session == "" {
		args = []string{"new", "host-shell", "-c", dir, "--json"}
	}
	b, err := term.rex(ctx, append(args, command...)...)
	if err != nil {
		return Session{}, err
	}
	var v struct {
		Session string   `json:"session_id"`
		Window  string   `json:"window_id"`
		Blocks  []string `json:"block_ids"`
		Windows []struct {
			Window string   `json:"window_id"`
			Blocks []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return Session{}, fmt.Errorf("shell may have opened; inspect Rex before retrying: %w", err)
	}
	if session == "" {
		session = v.Session
		if len(v.Windows) > 0 {
			v.Window, v.Blocks = v.Windows[0].Window, v.Windows[0].Blocks
		}
	}
	opened := Session{Backend: "rex", TerminalPath: term.path, Endpoint: term.endpoint, ID: session, WindowID: v.Window}
	if len(v.Blocks) > 0 {
		opened.BlockID = v.Blocks[0]
	}
	if opened.ID == "" || opened.WindowID == "" {
		return opened, fmt.Errorf("shell may have opened; Rex returned incomplete metadata; inspect Rex before retrying")
	}
	return opened, term.selectRexSession(ctx, opened, client)
}

const installSandboxShell = `import glob,os,pathlib,sys
root=pathlib.Path('/sandbox/.paddock')
(root/'bin').mkdir(parents=True,exist_ok=True,mode=0o700)
p=root/'bin/paddock'
p.write_text(sys.argv[1]); p.chmod(0o700)
line='\n# Paddock host-shell shortcut\nexport PATH="/sandbox/.paddock/bin:$PATH"\n'
home=pathlib.Path('/home/sandbox')
files=[home/n for n in ('.profile','.bashrc','.zprofile','.zshrc')]
files.extend(home/n for n in ('.bash_profile','.bash_login') if (home/n).exists())
for pattern in ('.codex/shell_snapshots/*.sh','.claude/shell-snapshots/*.sh'):
    files.extend(home.glob(pattern))
for f in files:
    if f.is_symlink(): continue
    text=f.read_text() if f.exists() else ''
    if '# Paddock host-shell shortcut' not in text:
        with f.open('a') as out: out.write(line)
`

func installHostShellShortcut(ctx context.Context, t Task) error {
	_, err := sandboxOutput(ctx, t, "python3", "-c", installSandboxShell, sandboxShellClient)
	return err
}

func (s Service) EnableHostShell(ctx context.Context, t Task) error {
	if t.RuntimeName() != "openshell" || t.OpenShell == nil || !t.OpenShell.Ready || t.Session.Backend != "rex" {
		return fmt.Errorf("host shell shortcut requires a ready OpenShell task in Rex")
	}
	if err := installHostShellShortcut(ctx, t); err != nil {
		return err
	}
	return s.ensureRuntimePoller(ctx)
}

const readShellRequest = `import os,pathlib,re,stat,sys
root=pathlib.Path('/sandbox/.paddock/shell-requests')
try:
    names=sorted(os.listdir(root))
except FileNotFoundError:
    sys.exit(0)
for name in names:
    if not re.fullmatch('[0-9a-f]{24}',name): continue
    try:
        fd=os.open(root/name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
        with os.fdopen(fd,'rb') as f:
            if not stat.S_ISREG(os.fstat(f.fileno()).st_mode): continue
            if f.read(128).decode('ascii') == name:
                print(name); break
    except (OSError,UnicodeError): continue
`

const acknowledgeShellRequest = `import os,pathlib,secrets,sys
root=pathlib.Path('/sandbox/.paddock')
p=root/('shell-response-'+sys.argv[1])
temp=root/('.response-'+secrets.token_hex(12))
fd=os.open(temp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,'w') as f: f.write(sys.argv[2])
os.replace(temp,p)
(root/'shell-requests'/sys.argv[1]).unlink(missing_ok=True)
`

type shellRequestResult struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"`
	Session   Session   `json:"session"`
	Error     string    `json:"error,omitempty"`
}

func (s Service) handleShellRequest(ctx context.Context, t Task, id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid host shell request")
	}
	release, err := s.Store.taskLease(t.ID, "shell")
	if err != nil {
		return err
	}
	defer release()
	dir := filepath.Join(s.Store.Home, "shell-requests")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file := filepath.Join(dir, t.ID+".json")
	var saved shellRequestResult
	b, err := os.ReadFile(file)
	if err == nil {
		if err = json.Unmarshal(b, &saved); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if saved.ID != id {
		if !saved.CreatedAt.IsZero() && time.Since(saved.CreatedAt) < 10*time.Second {
			_, err = sandboxOutput(ctx, t, "python3", "-c", acknowledgeShellRequest, id, "Please wait ten seconds before opening another host shell.")
			return err
		}
		saved = shellRequestResult{ID: id, CreatedAt: time.Now(), Status: "opening"}
		// Record intent before creating the tab. A crash must not create a second
		// tab when the singleton retries the same request.
		if err = writeJSONAtomic(file, saved); err != nil {
			return err
		}
		saved.Session, err = s.OpenHostShell(ctx, &t, "")
		saved.Status = "opened"
		if err != nil {
			saved.Status, saved.Error = "failed", err.Error()
		}
		if e := writeJSONAtomic(file, saved); e != nil {
			return e
		}
	}
	message := "opened"
	if saved.Status == "opening" {
		message = "Shell creation was interrupted. Inspect Rex before requesting another tab."
	}
	if saved.Error != "" {
		message = saved.Error
	}
	_, err = sandboxOutput(ctx, t, "python3", "-c", acknowledgeShellRequest, id, message)
	return err
}

// RunShellRequests shares the existing host singleton. The sandbox can request
// only a shell for its own task; no supplied command, client, or path is executed.
func (s Service) RunShellRequests(ctx context.Context) {
	installed := map[string]bool{}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		tasks, err := s.Store.List("")
		if err == nil {
			for _, t := range tasks {
				if t.RuntimeName() != "openshell" || t.OpenShell == nil || !t.OpenShell.Ready || t.Session.Backend != "rex" || t.Status != "running" {
					continue
				}
				pass, cancel := context.WithTimeout(ctx, 10*time.Second)
				key := fmt.Sprintf("%s/%d", t.ID, t.Session.Generation)
				if !installed[key] {
					installed[key] = installHostShellShortcut(pass, t) == nil
				}
				id, e := sandboxOutput(pass, t, "python3", "-c", readShellRequest)
				if e == nil && validID(id) {
					_ = s.handleShellRequest(pass, t, id)
				}
				cancel()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
