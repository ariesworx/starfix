package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultSocket is where starfixd listens unless configured otherwise.
const DefaultSocket = "/run/starfix/starfixd.sock"

// Listen opens the daemon's unix socket at path. The directory is created
// 0700 if missing and refused if anyone but its owner can enter it; the
// socket itself is 0600. A stale socket is removed; a live one (another
// daemon) is an error.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("socket directory %s is mode %04o; fix: chmod 700 %s", dir, fi.Mode().Perm(), dir)
	}
	if err := ownedByMe(fi); err != nil {
		return nil, fmt.Errorf("socket directory %s: %w", dir, err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket; fix: move it away", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another starfixd is listening on %s; fix: stop it first", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("socket: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("chmod socket: %w", err), l.Close())
	}
	return l, nil
}
