package client

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// iapConn is the SSH transport through Google Cloud IAP: the stdin and
// stdout of `gcloud compute start-iap-tunnel --listen-on-stdin`. IAP only
// carries the bytes; SSH runs over them unchanged, host-key pin included.
type iapConn struct {
	iap    *IAP
	port   int
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *capBuffer
	once   sync.Once
}

// iapArgs are gcloud's arguments for a tunnel to port on the instance.
// Every value in them was validated by ParseConfig against Google Cloud's
// naming rules, so none can pass for a flag or carry a shell
// metacharacter.
func iapArgs(iap *IAP, port int) []string {
	return []string{"compute", "start-iap-tunnel", iap.Instance, strconv.Itoa(port), "--listen-on-stdin",
		"--project=" + iap.Project, "--zone=" + iap.Zone, "--verbosity=warning"}
}

// dialIAP starts gcloud's tunnel to the instance cfg names.
//
// The program is always gcloud, found on PATH, and never a command from
// the config: .starfix.yaml is committed, so a command there would be code
// execution from a cloned repository. exec.LookPath also refuses a gcloud
// in the current directory (exec.ErrDot), so a checkout cannot plant one.
// On Windows it finds gcloud.cmd, which os/exec runs through cmd.exe; that
// is safe here only because iapArgs holds validated tokens.
//
// The tunnel outlives Dial's context, so it is not tied to one; Dial
// closes it if the context ends first, and Close kills and reaps it.
func dialIAP(cfg *Config) (*iapConn, error) {
	path, err := gcloud()
	if err != nil {
		return nil, err
	}
	c := &iapConn{iap: cfg.Server.IAP, port: cfg.Server.Port, stderr: &capBuffer{max: 4096, tail: true}}
	c.cmd = exec.Command(path, iapArgs(c.iap, c.port)...) //nolint:gosec // gcloud from PATH, with validated arguments
	c.cmd.Stderr = c.stderr
	// A grandchild holding stderr open must not stall Close.
	c.cmd.WaitDelay = time.Second
	if c.stdin, err = c.cmd.StdinPipe(); err != nil {
		return nil, fmt.Errorf("gcloud stdin: %w", err)
	}
	if c.stdout, err = c.cmd.StdoutPipe(); err != nil {
		return nil, fmt.Errorf("gcloud stdout: %w", err)
	}
	if err := c.cmd.Start(); err != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "check that `gcloud version` runs",
			fmt.Sprintf("cannot start %s: %v", path, err))
	}
	return c, nil
}

// gcloud finds gcloud on PATH.
func gcloud() (string, error) {
	path, err := exec.LookPath("gcloud")
	if err != nil {
		return "", proto.Errf(proto.CodeUnavailable,
			"install the Google Cloud CLI (https://cloud.google.com/sdk/docs/install), then run `gcloud auth login`",
			fmt.Sprintf("server.iap in %s needs gcloud, which is not on PATH", ConfigFile))
	}
	return path, nil
}

// CheckTransport reports, without connecting, a program this machine lacks
// to reach the server: gcloud, when the server is behind IAP. It returns
// a *proto.Error with a fix, or nil.
func (c *Config) CheckTransport() error {
	if c.Server.IAP == nil {
		return nil
	}
	_, err := gcloud()
	return err
}

func (c *iapConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *iapConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close ends the tunnel: stdin first, which is how gcloud learns to stop
// (and all that reaches its Python child on Windows, where Kill stops only
// cmd.exe), then a kill, then Wait, which reaps it and ends the goroutine
// copying its stderr.
func (c *iapConn) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait() // a killed tunnel's exit status says nothing more
	})
	return nil
}

func (c *iapConn) LocalAddr() net.Addr  { return iapAddr("localhost") }
func (c *iapConn) RemoteAddr() net.Addr { return iapAddr(c.String()) }

// Pipes from a child have no portable deadlines; Dial bounds the
// handshake by closing the connection instead.
func (c *iapConn) SetDeadline(time.Time) error      { return os.ErrNoDeadline }
func (c *iapConn) SetReadDeadline(time.Time) error  { return os.ErrNoDeadline }
func (c *iapConn) SetWriteDeadline(time.Time) error { return os.ErrNoDeadline }

// String names the far end for messages.
func (c *iapConn) String() string {
	return fmt.Sprintf("instance %s (project %s, zone %s)", c.iap.Instance, c.iap.Project, c.iap.Zone)
}

// failed explains a tunnel that ended before SSH got through it, quoting
// what gcloud printed. Call it after Close, so stderr is complete.
func (c *iapConn) failed(err error, timeout time.Duration) *proto.Error {
	// The same tunnel on a local port, which a person can run to see it.
	try := fmt.Sprintf("`gcloud compute start-iap-tunnel %s %d --project=%s --zone=%s`",
		c.iap.Instance, c.port, c.iap.Project, c.iap.Zone)
	said := quoteStderr(c.stderr.String(), "gcloud")
	if timeout > 0 {
		return proto.Errf(proto.CodeUnavailable,
			"check that the instance is running and that "+try+" connects; if it does not, run `gcloud auth login`",
			fmt.Sprintf("timed out after %s reaching %s through IAP%s", timeout, c, said))
	}
	return proto.Errf(proto.CodeUnavailable,
		"ask the starfix admin for the IAP-secured Tunnel User role on the instance, run `gcloud auth login`, then try "+try,
		fmt.Sprintf("the IAP tunnel to %s closed before the SSH handshake: %v%s", c, err, said))
}

// iapAddr is an IAP tunnel's address, for net.Conn.
type iapAddr string

func (iapAddr) Network() string  { return "iap" }
func (a iapAddr) String() string { return string(a) }
