package paddock

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func verifyClaudePeer(conn *net.UnixConn, pid int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		peerErr = e
		if e == nil && (int(cred.Pid) != pid || int(cred.Uid) != os.Getuid()) {
			peerErr = fmt.Errorf("Claude inbox process or owner does not match")
		}
	})
	if err != nil {
		return err
	}
	return peerErr
}
