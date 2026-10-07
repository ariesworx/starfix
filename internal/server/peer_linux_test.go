//go:build linux

package server

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// socketPair returns both ends of a connected unix socket pair, as
// *net.UnixConn.
func socketPair(t *testing.T) (a, b net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int, name string) net.Conn {
		f := os.NewFile(uintptr(fd), name)
		defer func() { _ = f.Close() }()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	return conn(fds[0], "a"), conn(fds[1], "b")
}

func TestCheckPeer(t *testing.T) {
	pipeA, _ := net.Pipe()
	t.Cleanup(func() { _ = pipeA.Close() })
	unixA, _ := socketPair(t)
	me := os.Getuid()
	tests := []struct {
		name string
		c    net.Conn
		uid  int // the uid the daemon runs as
		err  string
	}{
		{name: "same uid", c: unixA, uid: me},
		{name: "other uid", c: unixA, uid: me + 1, err: "is not the daemon's uid"},
		{name: "not a unix socket", c: pipeA, uid: me, err: "not a unix socket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPeerUID(tc.c, tc.uid)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("checkPeerUID(%s, %d) = %v, want nil", tc.name, tc.uid, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("checkPeerUID(%s, %d) = %v, want an error containing %q", tc.name, tc.uid, err, tc.err)
			}
		})
	}
	if err := CheckPeer(unixA); err != nil {
		t.Errorf("CheckPeer(own socket pair) = %v, want nil", err)
	}
}

// TestPeerHelperProcess is TestCheckPeerOtherUID's child: it dials the
// address in peerHelperEnv and waits for the daemon side to hang up.
func TestPeerHelperProcess(t *testing.T) {
	addr := os.Getenv(peerHelperEnv)
	if addr == "" {
		t.Skip("helper process for TestCheckPeerOtherUID")
	}
	c, err := net.Dial("unix", addr) //nolint:gosec // the parent test's own socket
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, c)
}

// TestCheckPeerOtherUID connects from a process running as another user,
// through a real socket, and checks that CheckPeer refuses it. Changing
// uid needs root; CI runs this test under sudo.
func TestCheckPeerOtherUID(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to run a peer as another uid")
	}
	const nobody = 65534
	// The child runs as nobody, so it needs a binary and a socket it can
	// reach: copy this test binary to a world-readable directory, and use
	// an abstract socket, which has no file permissions.
	dir, err := os.MkdirTemp("", "sfpeer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the child runs as another user
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "peer.test")
	b, err := os.ReadFile(self) //nolint:gosec // this test binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil { //nolint:gosec // the child runs as another user
		t.Fatal(err)
	}
	addr := "@starfix-peer-test-" + filepath.Base(dir)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestPeerHelperProcess$") //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(), peerHelperEnv+"="+addr)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody}}
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() { c, err := l.Accept(); ch <- accepted{c, err} }()
	var c net.Conn
	select {
	case a := <-ch:
		if a.err != nil {
			t.Fatal(a.err)
		}
		c = a.c
	case <-ctx.Done():
		t.Fatalf("the peer never connected:\n%s", out)
	}
	err = CheckPeer(c)
	_ = c.Close()
	if werr := cmd.Wait(); werr != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Logf("peer: %v\n%s", werr, out)
	}
	if err == nil || !strings.Contains(err.Error(), "peer uid 65534") {
		t.Fatalf("CheckPeer(connection from uid %d) = %v, want a refusal naming peer uid %d", nobody, err, nobody)
	}
}
