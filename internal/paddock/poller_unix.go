//go:build !windows

package paddock

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"syscall"
)

func detachPoller(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func tryPollerReadLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
