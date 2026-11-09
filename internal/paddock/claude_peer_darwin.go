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
		var peerPID int
		peerPID, peerErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if peerErr != nil {
			return
		}
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		peerErr = e
		if e == nil && (peerPID != pid || int(cred.Uid) != os.Getuid()) {
			peerErr = fmt.Errorf("Claude inbox process or owner does not match")
		}
	})
	if err != nil {
		return err
	}
	return peerErr
}
