//go:build !windows

package paddock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func clipboardTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func openClipboardImage(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func pasteInput(ctx context.Context, input *os.File, filter *clipboardInput) error {
	buffer := make([]byte, 32<<10)
	for ctx.Err() == nil {
		poll := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
		wait := 100
		if len(filter.pending) > 0 {
			wait = 10
		}
		n, err := unix.Poll(poll, wait)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			// Do not hold an ordinary typed slash, Escape, or incomplete path
			// until the next keystroke. Pasted paths arrive as a burst.
			if err = filter.Flush(); err != nil {
				return err
			}
			continue
		}
		count, err := unix.Read(int(input.Fd()), buffer)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return filter.Flush()
		}
		if _, err = filter.Write(buffer[:count]); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// A private PTY preserves OpenShell's native raw mode, resize handling, and
// remote TUI while this foreground helper rewrites only selected image paths.
func runPasteTerminal(ctx context.Context, command *exec.Cmd, upload func(context.Context, string) (string, error)) (int, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return 1, fmt.Errorf("image paste transport requires a terminal")
	}
	size, err := pty.GetsizeFull(os.Stdin)
	if err != nil {
		return 1, fmt.Errorf("inspect terminal size: %w", err)
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return 1, fmt.Errorf("prepare terminal: %w", err)
	}
	defer term.Restore(fd, state)
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	master, err := pty.StartWithSize(command, size)
	if err != nil {
		return 1, fmt.Errorf("start sandbox terminal: %w", err)
	}
	defer master.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	inputDone := make(chan error, 1)
	go func() {
		inputDone <- pasteInput(ctx, os.Stdin, newClipboardInput(master, clipboardDirectories(), func(source string) (string, error) { return upload(ctx, source) }))
	}()
	outputDone := make(chan struct{})
	go func() { defer close(outputDone); _, _ = io.Copy(os.Stdout, master) }()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var terminalError error
	var terminate <-chan time.Time
	cancelled := ctx.Done()
	inputFinished := false
wait:
	for {
		select {
		case <-done:
			break wait
		case <-cancelled:
			_ = unix.Kill(-command.Process.Pid, syscall.SIGTERM)
			terminate = time.After(2 * time.Second)
			// Avoid selecting a closed cancellation channel repeatedly.
			cancelled = nil
		case err := <-inputDone:
			inputFinished = true
			if err != nil && !errors.Is(err, context.Canceled) {
				terminalError = fmt.Errorf("read terminal input: %w", err)
			}
			_ = unix.Kill(-command.Process.Pid, syscall.SIGHUP)
			terminate = time.After(2 * time.Second)
		case sig := <-signals:
			if sig == syscall.SIGWINCH {
				if current, err := pty.GetsizeFull(os.Stdin); err == nil {
					_ = pty.Setsize(master, current)
				}
				continue
			}
			if native, ok := sig.(syscall.Signal); ok {
				_ = unix.Kill(-command.Process.Pid, native)
			}
			if sig != syscall.SIGINT {
				terminate = time.After(2 * time.Second)
			}
		case <-terminate:
			_ = unix.Kill(-command.Process.Pid, syscall.SIGKILL)
			terminate = nil
		}
	}
	cancel()
	if !inputFinished {
		select {
		case <-inputDone:
		case <-time.After(time.Second):
		}
	}
	select {
	case <-outputDone:
	case <-time.After(time.Second):
	}
	code := command.ProcessState.ExitCode()
	if code < 0 {
		code = 128
	}
	return code, terminalError
}
