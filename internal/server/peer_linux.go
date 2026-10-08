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
func CheckPeer(c net.Conn) error { return checkPeerUID(c, os.Getuid()) }

// checkPeerUID refuses c unless it is a unix socket whose peer runs as uid.
func checkPeerUID(c net.Conn, uid int) error {
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
	if int(cred.Uid) != uid {
		return fmt.Errorf("peer uid %d is not the daemon's uid %d", cred.Uid, uid)
	}
	return nil
}

// ownedByMe refuses a file another user owns, such as a socket directory
// whose owner could replace the socket. A FileInfo without a Stat_t is
// accepted.
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
