//go:build linux

package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
)

// PeerChecked reports whether CheckPeer verifies the connecting user.
const PeerChecked = true

// CheckPeer refuses a unix socket connection from any user but the
// daemon's own, using SO_PEERCRED.
func CheckPeer(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer credentials: %w", err)
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED) //nolint:gosec // fd fits in int
	}); err != nil {
		return fmt.Errorf("peer credentials: %w", err)
	}
	if cerr != nil {
		return fmt.Errorf("peer credentials: %w", cerr)
	}
	if uid := os.Getuid(); int(cred.Uid) != uid {
		return fmt.Errorf("peer uid %d is not the daemon's uid %d", cred.Uid, uid)
	}
	return nil
}

func ownedByMe(fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("owned by uid %d, not the daemon's uid %d; fix: chown it to the starfixd user", st.Uid, uid)
	}
	return nil
}
