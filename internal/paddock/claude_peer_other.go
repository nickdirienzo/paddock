//go:build !darwin && !linux

package paddock

import (
	"fmt"
	"net"
)

func verifyClaudePeer(*net.UnixConn, int) error {
	return fmt.Errorf("Claude host inbox peer verification requires macOS or Linux")
}
