//go:build !windows

package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestClipboardTerminalHelper(t *testing.T) {
	if os.Getenv("PADDOCK_TEST_CLIPBOARD_PTY") != "1" {
		return
	}
	python := os.Getenv("PADDOCK_TEST_CLIPBOARD_PYTHON")
	script := `import json,os,signal,struct,sys,termios,tty
tty.setraw(0)
def size(*args):
    rows,cols,_,_=struct.unpack('HHHH',__import__('fcntl').ioctl(0,termios.TIOCGWINSZ,b'\0'*8))
    print('SIZE:'+str(rows)+','+str(cols),flush=True)
signal.signal(signal.SIGWINCH,size)
size()
print('READY',flush=True)
data=b''
while b'\x1b[201~' not in data:
    chunk=os.read(0,4096)
    if not chunk:sys.exit(2)
    data+=chunk
print('INPUT:'+json.dumps(data.decode()),flush=True)
sys.exit(7)
`
	command := exec.Command(python, "-u", "-c", script)
	code, err := runPasteTerminal(context.Background(), command, func(_ context.Context, source string) (string, error) {
		if filepath.Base(source) != "codex-clipboard-abc123.png" {
			return "", fmt.Errorf("unexpected clipboard source")
		}
		return "/sandbox/.paddock/attachments/test.png", nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func TestClipboardTerminalPreservesResizeInputAndExit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClipboardTerminalHelper$")
	command.Env = append(os.Environ(), "PADDOCK_TEST_CLIPBOARD_PTY=1", "PADDOCK_TEST_CLIPBOARD_PYTHON="+python)
	outer, err := pty.StartWithSize(command, &pty.Winsize{Rows: 25, Cols: 91})
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	defer command.Process.Kill()
	scanner := bufio.NewScanner(outer)
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			lines <- strings.TrimSpace(scanner.Text())
		}
	}()
	wait := func(prefix string) string {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatal("terminal disconnected before " + prefix)
				}
				if strings.HasPrefix(line, prefix) {
					return line
				}
			case <-ctx.Done():
				t.Fatal("terminal timed out before " + prefix)
			}
		}
	}
	if line := wait("SIZE:"); line != "SIZE:25,91" {
		t.Fatal("initial size not propagated", line)
	}
	wait("READY")
	if err = pty.Setsize(outer, &pty.Winsize{Rows: 42, Cols: 120}); err != nil {
		t.Fatal(err)
	}
	if line := wait("SIZE:"); line != "SIZE:42,120" {
		t.Fatal("resize not propagated", line)
	}
	input := "\x1b[200~look at " + filepath.Join(os.TempDir(), "codex-clipboard-abc123.png") + "\x1b[201~"
	if _, err = outer.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	line := wait("INPUT:")
	var actual string
	if json.Unmarshal([]byte(strings.TrimPrefix(line, "INPUT:")), &actual) != nil || actual != "\x1b[200~look at /sandbox/.paddock/attachments/test.png\x1b[201~" {
		t.Fatalf("incorrect native input: %s", line)
	}
	err = command.Wait()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Fatalf("native exit code not retained: %v", err)
	}
}

func TestLiveClipboardTerminalHelper(t *testing.T) {
	id := os.Getenv("PADDOCK_TEST_LIVE_CLIPBOARD_PTY")
	if id == "" {
		return
	}
	home, err := StateHome()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	task.Worktree = "/tmp"
	script := `import hashlib,os,pathlib,tty
tty.setraw(0)
print('IMAGE_READY',flush=True)
data=b''
while b'\x1b[201~' not in data:
    chunk=os.read(0,4096)
    if not chunk:raise RuntimeError('terminal disconnected')
    data+=chunk
source=pathlib.Path(data.decode().removeprefix('\x1b[200~').removesuffix('\x1b[201~'))
print('IMAGE_SHA256:'+hashlib.sha256(source.read_bytes()).hexdigest(),flush=True)
source.unlink()
`
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := sandboxExec(ctx, task, true, "/usr/bin/python3", "-u", "-c", script)
	code, err := runPasteTerminal(ctx, command, func(ctx context.Context, source string) (string, error) {
		return uploadClipboardImage(ctx, task, source)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func exerciseLiveClipboardTerminal(t *testing.T, id, source, wantHash string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLiveClipboardTerminalHelper$")
	command.Env = append(os.Environ(), "PADDOCK_TEST_LIVE_CLIPBOARD_PTY="+id)
	outer, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	defer command.Process.Kill()
	scanner := bufio.NewScanner(outer)
	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			lines <- strings.TrimSpace(scanner.Text())
		}
	}()
	matched := false
read:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break read
			}
			if line == "IMAGE_READY" {
				if _, err = outer.Write([]byte("\x1b[200~" + source + "\x1b[201~")); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(line, "IMAGE_SHA256:") {
				matched = line == "IMAGE_SHA256:"+wantHash
			}
		case <-ctx.Done():
			t.Fatal("live clipboard terminal timed out")
		}
	}
	if err = command.Wait(); err != nil {
		t.Fatal("live clipboard terminal failed", err)
	}
	if !matched {
		t.Fatal("pasted image did not reach the real sandbox terminal with its exact bytes")
	}
	t.Log("Pasted image crossed the terminal bridge and was read inside the real sandbox")
}
