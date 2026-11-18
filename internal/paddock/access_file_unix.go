//go:build !windows

package paddock

import (
	"golang.org/x/sys/unix"
	"os"
)

func openAccessConfigFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
