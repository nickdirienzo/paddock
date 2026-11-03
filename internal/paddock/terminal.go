package paddock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Terminal interface {
	Launch(context.Context, Task, string, string) (Session, error)
	Inspect(context.Context, Task) (Session, Observation)
	Attach(context.Context, Task, string) error
}

type terminal struct{ backend, path, endpoint string }

func executable(env, name string) (string, error) {
	if override := os.Getenv(env); override != "" {
		name = override
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func rexExecutable() (string, error) {
	if p, err := executable("PADDOCK_REX_BIN", "rex"); err == nil {
		return p, nil
	}
	if os.Getenv("PADDOCK_REX_BIN") == "" && runtime.GOOS == "darwin" {
		for _, app := range []string{"Rex Beta.app", "Rex.app"} {
			p := filepath.Join("/Applications", app, "Contents/Helpers/Rex Server.rex/Contents/MacOS/rex")
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("Rex executable not found; set PADDOCK_REX_BIN")
}

func output(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.Env = credentialFreeEnvironment(os.Environ())
	var stderr bytes.Buffer
	c.Stderr = &stderr
	b, err := c.Output()
	if err != nil {
		return b, fmt.Errorf("%s: %w: %s", filepath.Base(path), err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

func (t terminal) rex(ctx context.Context, args ...string) ([]byte, error) {
	flags := []string{"--autostart=false", "--timeout", "10s"}
	if t.endpoint != "" {
		flags = append(flags, "-S", t.endpoint)
	}
	return output(ctx, t.path, append(flags, args...)...)
}

func SelectTerminal(ctx context.Context, backend string) (Terminal, Session, error) {
	var reasons []string
	if backend == "" || backend == "auto" || backend == "rex" {
		p, err := rexExecutable()
		if err == nil {
			t := terminal{backend: "rex", path: p}
			var b []byte
			b, err = t.rex(ctx, "whoami", "--json")
			if err == nil {
				var v struct {
					Endpoint string `json:"endpoint"`
				}
				err = json.Unmarshal(b, &v)
				if err == nil && !strings.HasPrefix(v.Endpoint, "unix://") {
					err = fmt.Errorf("v0 requires a local Rex server; got %q", v.Endpoint)
				}
				if err == nil {
					t.endpoint = v.Endpoint
					return t, Session{Backend: "rex", TerminalPath: p, Endpoint: v.Endpoint}, nil
				}
			}
		}
		reasons = append(reasons, "Rex: "+err.Error())
		if backend == "rex" {
			return nil, Session{}, err
		}
	}
	if backend == "" || backend == "auto" || backend == "screen" {
		p, err := executable("PADDOCK_SCREEN_BIN", "screen")
		if err == nil {
			var version []byte
			version, err = output(ctx, p, "--version")
			// macOS's bundled Screen prints a valid version but exits with 1.
			if strings.HasPrefix(string(version), "Screen version ") {
				err = nil
			}
		}
		if err == nil {
			return terminal{backend: "screen", path: p}, Session{Backend: "screen", TerminalPath: p}, nil
		}
		reasons = append(reasons, "Screen: "+err.Error())
	}
	if len(reasons) == 0 {
		return nil, Session{}, fmt.Errorf("unknown terminal %q (use auto, rex, or screen)", backend)
	}
	return nil, Session{}, fmt.Errorf("no terminal available; install/start Rex or install GNU Screen: %s", strings.Join(reasons, "; "))
}

func terminalFor(s Session) Terminal {
	return terminal{backend: s.Backend, path: s.TerminalPath, endpoint: s.Endpoint}
}

func launchCommand(exe, home, id string) []string {
	args := []string{"/usr/bin/env", "PADDOCK_HOME=" + home, "PATH=" + filepath.Dir(exe) + string(os.PathListSeparator) + os.Getenv("PATH")}
	// Explicit non-secret configuration is forwarded to Rex's server environment.
	for _, key := range []string{"PADDOCK_CODEX_BIN", "PADDOCK_CLAUDE_BIN", "PADDOCK_REX_BIN", "PADDOCK_SCREEN_BIN", "PADDOCK_MISE_BIN", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "MISE_STATE_DIR", "MISE_CONFIG_DIR", "XDG_STATE_HOME"} {
		if v := os.Getenv(key); v != "" {
			args = append(args, key+"="+v)
		}
	}
	return append(args, exe, "__run", id)
}

func (t terminal) Launch(ctx context.Context, task Task, exe, home string) (Session, error) {
	s := task.Session
	command := launchCommand(exe, home, task.ID)
	command = append(command, "--generation", strconv.Itoa(s.Generation))
	if t.backend == "rex" {
		dir := task.Worktree
		if task.RuntimeName() == "openshell" {
			dir = home
		}
		args := []string{"new", s.Name, "-c", dir, "--shell=none", "--json", "--"}
		b, err := t.rex(ctx, append(args, command...)...)
		if err != nil {
			return s, err
		}
		var v struct {
			ID      string `json:"session_id"`
			Windows []struct {
				ID     string   `json:"window_id"`
				Blocks []string `json:"block_ids"`
			} `json:"initial_windows"`
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return s, err
		}
		if v.ID == "" || len(v.Windows) == 0 || len(v.Windows[0].Blocks) == 0 {
			return s, fmt.Errorf("Rex returned incomplete session metadata")
		}
		s.ID = v.ID
		s.WindowID = v.Windows[0].ID
		s.BlockID = v.Windows[0].Blocks[0]
		return s, nil
	}
	// Ignore personal screenrc commands; they can replace the requested program.
	args := []string{"-c", os.DevNull, "-dmS", s.Name}
	if _, err := output(ctx, t.path, append(args, command...)...); err != nil {
		return s, err
	}
	s.ID = s.Name
	return s, nil
}

var screenLine = regexp.MustCompile(`(?m)^\s*([0-9]+)\.([^\s]+)\s+.*\((Detached|Attached)\)`)

func (t terminal) Inspect(ctx context.Context, task Task) (Session, Observation) {
	s := task.Session
	unknown := func(err error) (Session, Observation) { return s, Observation{State: "unknown", Error: err.Error()} }
	if t.backend == "screen" {
		b, err := output(ctx, t.path, "-ls")
		// Screen versions disagree on exit codes, including 1 for a valid list.
		matches := screenLine.FindAllStringSubmatch(string(b), -1)
		for _, m := range matches {
			if m[2] == s.Name {
				s.ID = m[1] + "." + m[2]
				return s, Observation{State: "running"}
			}
		}
		if strings.HasPrefix(strings.TrimSpace(string(b)), "No Sockets found in ") || len(matches) > 0 {
			return s, Observation{State: "missing"}
		}
		if err == nil {
			err = fmt.Errorf("unrecognized screen listing")
		}
		return unknown(err)
	}
	if t.backend != "rex" {
		return unknown(fmt.Errorf("unknown backend %q", t.backend))
	}
	b, err := t.rex(ctx, "ls", "--json")
	if err != nil {
		return unknown(err)
	}
	var list struct {
		Sessions []struct {
			ID    string `json:"session_id"`
			Label string `json:"label"`
		} `json:"sessions"`
	}
	if err = json.Unmarshal(b, &list); err != nil {
		return unknown(err)
	}
	found := false
	for _, v := range list.Sessions {
		if (s.ID != "" && v.ID == s.ID) || (s.ID == "" && v.Label == s.Name) {
			s.ID = v.ID
			found = true
			break
		}
	}
	if !found {
		return s, Observation{State: "missing"}
	}
	if s.BlockID == "" {
		b, err = t.rex(ctx, "session", "inspect", s.ID, "--json")
		if err != nil {
			return unknown(err)
		}
		var v struct {
			Windows []struct {
				ID    string `json:"window_id"`
				Block string `json:"focused_block_id"`
			} `json:"windows"`
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return unknown(err)
		}
		if len(v.Windows) == 0 {
			return s, Observation{State: "missing"}
		}
		s.WindowID = v.Windows[0].ID
		s.BlockID = v.Windows[0].Block
	}
	b, err = t.rex(ctx, "block", "call", "-s", s.ID, "-b", s.BlockID, "process")
	if err != nil {
		// Rex may retain an empty session after closing its last pane. Confirm
		// absence using a successful listing, never by parsing an error string.
		listing, listErr := t.rex(ctx, "block", "ls", "-s", s.ID, "--json")
		var blocks struct {
			Blocks []struct {
				ID string `json:"block_id"`
			} `json:"blocks"`
		}
		if listErr == nil && json.Unmarshal(listing, &blocks) == nil && blocks.Blocks != nil {
			missing := true
			for _, block := range blocks.Blocks {
				if block.ID == "" || block.ID == s.BlockID {
					missing = false
				}
			}
			if missing {
				return s, Observation{State: "missing"}
			}
		}
		return unknown(err)
	}
	var v struct {
		Child    json.RawMessage `json:"child"`
		LastExit *struct {
			Code *int `json:"exit_code"`
		} `json:"last_exit"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return unknown(err)
	}
	if len(v.Child) > 0 && string(v.Child) != "null" {
		return s, Observation{State: "running"}
	}
	if v.LastExit != nil {
		return s, Observation{State: "exited"}
	}
	return unknown(fmt.Errorf("Rex returned no process or exit status"))
}

func (t terminal) Attach(ctx context.Context, task Task, client string) error {
	s, obs := t.Inspect(ctx, task)
	if obs.State == "missing" {
		return fmt.Errorf("terminal session is gone; worktree remains at %s", task.Worktree)
	}
	if obs.State == "unknown" {
		return fmt.Errorf("cannot inspect terminal: %s", obs.Error)
	}
	if t.backend == "screen" {
		c := exec.CommandContext(ctx, t.path, "-x", s.ID)
		c.Env = credentialFreeEnvironment(os.Environ())
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}
	if client == "" {
		client = os.Getenv("REX_CLIENT")
	}
	if client == "" {
		b, err := t.rex(ctx, "client", "ls", "--json")
		if err != nil {
			return err
		}
		var v struct {
			Clients []struct {
				ID    string `json:"client_id"`
				State string `json:"connection_state"`
				Info  struct {
					Kind  string `json:"kind"`
					Label string `json:"label"`
				} `json:"info"`
			} `json:"clients"`
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return err
		}
		var choices []string
		for _, c := range v.Clients {
			if c.Info.Kind == "app" && c.State == "connected" {
				client = c.ID
				choices = append(choices, c.ID+" ("+c.Info.Label+")")
			}
		}
		if len(choices) != 1 {
			return fmt.Errorf("choose a connected Rex app with --client <id>; available: %s", strings.Join(choices, ", "))
		}
	}
	b, err := t.rex(ctx, "-C", client, "actions", "--json")
	if err != nil {
		return err
	}
	var actions struct {
		Actions []struct {
			Name string `json:"name"`
		} `json:"actions"`
	}
	if err = json.Unmarshal(b, &actions); err != nil {
		return err
	}
	supported := false
	for _, a := range actions.Actions {
		if a.Name == "session.select" {
			supported = true
		}
	}
	if !supported {
		return fmt.Errorf("this Rex app does not support session.select; open session %s manually", s.ID)
	}
	args := []string{"-C", client, "do", "session.select", "session_id=" + s.ID}
	if s.WindowID != "" {
		args = append(args, "window_id="+s.WindowID)
	}
	_, err = t.rex(ctx, args...)
	if err != nil {
		return fmt.Errorf("%w; enable Remote Control in Rex Settings > Rex Server, or open session %s manually", err, s.ID)
	}
	return nil
}
