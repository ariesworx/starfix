package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/proto"
)

// rawSession is a protocol session over SSH without the client package, so a
// test can send what the client never would.
type rawSession struct {
	t     *testing.T
	stdin io.WriteCloser
	enc   *proto.Encoder
	dec   *proto.Decoder
	nc    net.Conn
}

// rawDial connects as u, in session, and completes the handshake.
func (u *user) rawDial(session string) *rawSession {
	t := u.w.t
	t.Helper()
	b, err := os.ReadFile(filepath.Join(u.repo, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	hostFpr := u.w.hostFpr
	cfg := &ssh.ClientConfig{
		User: "starfix",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != hostFpr {
				return errors.New("host key mismatch")
			}
			return nil
		},
		Timeout: 10 * time.Second,
	}
	nc, err := net.DialTimeout("tcp", u.w.addr.String(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, u.w.addr.String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { _ = client.Close() })
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("starfixd stdio"); err != nil {
		t.Fatal(err)
	}
	r := &rawSession{t: t, stdin: stdin, enc: proto.NewEncoder(stdin), dec: proto.NewDecoder(stdout), nc: nc}
	if err := r.enc.Encode(&proto.Frame{T: proto.FrameHello, Version: "v0.2.0", Proto: proto.Proto,
		Project: project, Session: session, Machine: "laptop-test"}); err != nil {
		t.Fatal(err)
	}
	if w := r.next(); w.T != proto.FrameWelcome || w.Err != nil {
		t.Fatalf("handshake: %+v", w)
	}
	return r
}

// next reads the next frame, failing the test after 30 seconds.
func (r *rawSession) next() *proto.Frame {
	r.t.Helper()
	f, err := r.nextErr()
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	return f
}

// nextErr is next for a read that may fail: it returns the error.
func (r *rawSession) nextErr() (*proto.Frame, error) {
	_ = r.nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer func() { _ = r.nc.SetReadDeadline(time.Time{}) }()
	return r.dec.Decode()
}

// TestOversizedFrameAfterHandshake sends a request larger than
// proto.MaxFrame. The server answers with a typed invalid refusal and ends
// that session, and another session on the same daemon keeps working.
func TestOversizedFrameAfterHandshake(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "").rawDial("s-big")
	bob := w.newUser("bob", "").rawDial("s-bob")

	var big bytes.Buffer
	big.WriteString(`{"t":"req","id":7,"op":"show","a":{"id":"`)
	big.WriteString(strings.Repeat("a", proto.MaxFrame))
	big.WriteString("\"}}\n")
	written := make(chan error, 1)
	go func() {
		_, err := alice.stdin.Write(big.Bytes())
		written <- err
	}()

	f := alice.next()
	if f.T != proto.FrameRes || f.Err == nil || f.Err.Code != proto.CodeInvalid ||
		!strings.Contains(f.Err.Message, "larger than 4 MiB") || f.Err.Fix == "" {
		t.Fatalf("oversized request got %+v (err %+v), want an invalid refusal with a fix", f, f.Err)
	}
	if f, err := alice.nextErr(); err == nil {
		t.Fatalf("session stayed open after the oversized frame: got %+v", f)
	}
	_ = alice.stdin.Close()
	<-written // the write may fail once the server hangs up; either is fine

	if err := bob.enc.Encode(&proto.Frame{T: proto.FrameReq, ID: 1, Op: proto.OpWho, Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if f := bob.next(); f.T != proto.FrameRes || f.ID != 1 || f.Err != nil {
		t.Fatalf("other session after the oversized frame: %+v (err %+v)", f, f.Err)
	}
}
