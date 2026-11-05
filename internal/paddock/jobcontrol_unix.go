//go:build !windows

package paddock

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func prepareAgentTerminal(c *exec.Cmd) (func() error, bool, error) {
	noop := func() error { return nil }
	fd := int(os.Stdin.Fd())
	foreground, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENODEV) {
		return noop, false, nil
	}
	if err != nil {
		return noop, false, fmt.Errorf("inspect foreground terminal: %w", err)
	}
	if foreground != unix.Getpgrp() {
		return noop, false, fmt.Errorf("Paddock supervisor must run in the terminal foreground")
	}
	state, err := term.GetState(fd)
	if err != nil {
		return noop, false, fmt.Errorf("inspect terminal settings: %w", err)
	}
	// Go performs setpgid and tcsetpgrp before exec, so the agent can read the
	// terminal immediately and Rex sees the agent as the foreground leader.
	c.SysProcAttr = &syscall.SysProcAttr{Foreground: true, Ctty: fd}
	restore := func() error {
		// The supervisor is in the background until it reclaims the terminal.
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, foreground); err != nil {
			return err
		}
		return term.Restore(fd, state)
	}
	return restore, true, nil
}

func signalAgent(c *exec.Cmd, grouped bool, sig os.Signal) error {
	if grouped {
		if native, ok := sig.(syscall.Signal); ok {
			return unix.Kill(-c.Process.Pid, native)
		}
	}
	return c.Process.Signal(sig)
}
