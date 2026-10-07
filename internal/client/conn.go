package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
)

// Options describe this client to the server.
type Options struct {
	// Version is the client's build version.
	Version string
	// Session is the client session id; empty lets the server assign one.
	Session string
	// Machine is this machine's name.
	Machine string
	// Harness names the agent harness, or is empty (HarnessFromEnv).
	Harness string
	// Getenv reads SSH_AUTH_SOCK. Default os.Getenv.
	Getenv func(string) string
	// Timeout bounds connecting and the handshake. Default 15s.
	Timeout time.Duration
	// OnPush receives the events the server pushes (after a watch call),
	// in order, on the connection's reader goroutine. It must return
	// quickly and must not call the Conn: the next response waits for it.
	OnPush func(proto.Push)
}

// Conn is one session with starfixd. Calls are serialized. A reader
// goroutine reads every frame the server sends: responses go to the call
// waiting for them, pushed events to Options.OnPush, and frame types it
// does not know are skipped.
type Conn struct {
	// Welcome is the server's handshake reply.
	Welcome proto.Frame

	ssh    *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	enc    *proto.Encoder
	dec    *proto.Decoder
	stderr *capBuffer
	// kill closes the transport, ending the reader and any call.
	kill   func() error
	onPush func(proto.Push)

	res     chan *proto.Frame // responses, from the reader to Call
	done    chan struct{}     // closed when the reader stops
	readErr error             // why the reader stopped; set before done closes
	closed  chan struct{}     // closed by Close
	once    sync.Once

	mu     sync.Mutex
	nextID uint64
	broken error
}

// newConn returns a Conn over enc and dec, before its handshake.
func newConn(enc *proto.Encoder, dec *proto.Decoder, kill func() error, onPush func(proto.Push)) *Conn {
	return &Conn{enc: enc, dec: dec, kill: kill, onPush: onPush, stderr: &capBuffer{max: 4096},
		res: make(chan *proto.Frame, 1), done: make(chan struct{}), closed: make(chan struct{})}
}

// run starts the reader. Before it, the handshake reads frames itself.
func (c *Conn) run() { go c.read() }

// read delivers frames until the stream ends.
func (c *Conn) read() {
	defer close(c.done)
	for {
		f, err := c.dec.Decode()
		if err != nil {
			c.readErr = err
			return
		}
		switch f.T {
		case proto.FrameRes:
			select {
			case c.res <- f:
			case <-c.closed:
				return
			}
		case proto.FrameEvent:
			if p, err := proto.DecodePush(f); err == nil && c.onPush != nil {
				c.onPush(p)
			}
		}
	}
}

// Done is closed once the connection has ended, by Close or by the
// server or network.
func (c *Conn) Done() <-chan struct{} { return c.done }

// RemoteCommand is what the client asks sshd to run. The key's forced
// command overrides it anyway.
const RemoteCommand = "starfixd stdio"

// Dial connects to the server named in cfg, verifies its pinned host key,
// authenticates, and completes the version handshake. A refusal comes back
// as a *proto.Error.
func Dial(ctx context.Context, cfg *Config, opts Options) (*Conn, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	signers, closeAgent, err := signers(cfg, opts.Getenv)
	if err != nil {
		return nil, err
	}
	defer closeAgent()

	addr := net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
	var hostKeyErr *proto.Error
	sc := &ssh.ClientConfig{
		User:              cfg.Server.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != cfg.Server.HostKey {
				hostKeyErr = proto.Errf(proto.CodeAuth,
					"if the server was rebuilt, confirm its new key with the admin out of band, then update server.host_key; otherwise do not connect",
					fmt.Sprintf("host key mismatch for %s: the server presented %s, but %s pins %s", cfg.Server.Host, got, ConfigFile, cfg.Server.HostKey))
				return hostKeyErr
			}
			return nil
		},
		Timeout: opts.Timeout,
	}
	nc, err := dialTransport(ctx, cfg, addr)
	if err != nil {
		return nil, err
	}
	// Closing the transport bounds the SSH handshake for both transports;
	// the IAP tunnel's pipes have no portable deadlines.
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, sc)
	if err != nil {
		_ = nc.Close()
		if hostKeyErr != nil {
			return nil, hostKeyErr
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, proto.Errf(proto.CodeAuth,
				"send your public key (`ssh-add -L`, or the .pub next to your key file) to the starfix admin",
				fmt.Sprintf("%s refused your SSH key%s", addr, plural(len(signers))))
		}
		var timeout time.Duration
		if ctx.Err() != nil {
			timeout = opts.Timeout
		}
		if t, ok := nc.(*iapConn); ok {
			return nil, t.failed(err, timeout)
		}
		if timeout > 0 {
			return nil, proto.Errf(proto.CodeUnavailable, "check server.host and server.port in "+ConfigFile+", and your network",
				fmt.Sprintf("timed out after %s in the ssh handshake with %s", timeout, addr))
		}
		return nil, proto.Errf(proto.CodeUnavailable, "check server.host and server.port in "+ConfigFile,
			fmt.Sprintf("ssh handshake with %s failed: %v", addr, err))
	}
	client := ssh.NewClient(cc, chans, reqs)
	c, err := start(ctx, client, cfg, opts)
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	return c, nil
}

// dialTransport opens the byte stream SSH runs over: a TCP connection to
// addr, or, when cfg sets server.iap, a Google Cloud IAP tunnel.
func dialTransport(ctx context.Context, cfg *Config, addr string) (net.Conn, error) {
	if cfg.Server.IAP != nil {
		return dialIAP(cfg)
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "check server.host and server.port in "+ConfigFile+", and your network",
			fmt.Sprintf("cannot reach %s: %v", addr, err))
	}
	return nc, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return fmt.Sprintf("s (tried %d)", n)
}

func start(ctx context.Context, client *ssh.Client, cfg *Config, opts Options) (*Conn, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "retry; if it persists, ask the server admin", fmt.Sprintf("open ssh session: %v", err))
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout: %w", err)
	}
	c := newConn(proto.NewEncoder(stdin), proto.NewDecoder(stdout), client.Close, opts.OnPush)
	c.ssh, c.sess, c.stdin = client, sess, stdin
	sess.Stderr = c.stderr
	if err := sess.Start(RemoteCommand); err != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "check that this key's authorized_keys line forces `starfixd stdio --principal NAME`",
			fmt.Sprintf("the server would not start starfixd: %v", err))
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	if err := c.enc.Encode(&proto.Frame{T: proto.FrameHello, Version: opts.Version, Proto: proto.Proto,
		Project: cfg.Project, Session: opts.Session, Machine: opts.Machine, Harness: opts.Harness}); err != nil {
		// The bridge may have refused and closed before reading the hello;
		// its refusal is still waiting to be read.
		if w, rerr := c.next(); rerr == nil && w.T == proto.FrameWelcome && w.Err != nil {
			return nil, w.Err
		}
		return nil, c.lost(err)
	}
	w, err := c.next()
	if err != nil {
		return nil, c.lost(err)
	}
	if w.T != proto.FrameWelcome {
		return nil, proto.Errf(proto.CodeVersion, "upgrade starfix to match the server",
			fmt.Sprintf("expected a welcome from the server, got a %q frame", w.T))
	}
	if w.Err != nil {
		return nil, w.Err
	}
	if e := proto.CheckProto(proto.Proto, w.Min, w.Max, w.Version); e != nil {
		return nil, e
	}
	c.Welcome = *w
	c.run()
	return c, nil
}

// next reads, during the handshake, the next frame the client
// understands, skipping event frames and frame types added by newer
// servers.
func (c *Conn) next() (*proto.Frame, error) {
	for {
		f, err := c.dec.Decode()
		if err != nil {
			return nil, err
		}
		switch f.T {
		case proto.FrameWelcome, proto.FrameRes:
			return f, nil
		}
	}
}

// lost explains a connection that ended unexpectedly, quoting what the
// server printed on stderr.
func (c *Conn) lost(err error) *proto.Error {
	msg := "the server closed the connection"
	if !errors.Is(err, io.EOF) {
		msg = fmt.Sprintf("connection to the server failed: %v", err)
	}
	msg += quoteStderr(c.stderr.String(), "server")
	return proto.Errf(proto.CodeUnavailable,
		"retry; if it persists, check that this key's authorized_keys line forces `starfixd stdio --principal NAME` and that starfixd is running", msg)
}

// Session is the session id the server recorded for this connection.
func (c *Conn) Session() string { return c.Welcome.Session }

// Principal is who the server says this key belongs to. It is empty with a
// server older than the welcome's principal field.
func (c *Conn) Principal() string { return c.Welcome.Principal }

// Err reports why the connection can no longer be used, or nil while it
// can. A refused request does not break the connection; a lost one does,
// even while no call is running.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken == nil {
		select {
		case <-c.done:
			c.broken = c.lost(c.readErr)
		default:
		}
	}
	return c.broken
}

// Notices returns the one-line version notices for an agent or person: this
// client older than the server, and the server older than the latest
// release it knows of.
func (c *Conn) Notices(clientVersion string) []string {
	var out []string
	for _, n := range []string{
		proto.OlderClientWarning(clientVersion, c.Welcome.Version),
		proto.OlderServerWarning(c.Welcome.Version, c.Welcome.Latest),
	} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Warning returns a one-line notice for the person when this client is
// older than the server, or "".
func (c *Conn) Warning(clientVersion string) string {
	return proto.OlderClientWarning(clientVersion, c.Welcome.Version)
}

// Call sends one request and decodes its result into result (which may be
// nil). A server refusal is returned as a *proto.Error.
func (c *Conn) Call(ctx context.Context, op string, args, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	c.nextID++
	id := c.nextID
	f, err := proto.Request(id, op, args)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.kill() })
	defer stop()
	if err := c.enc.Encode(f); err != nil {
		c.broken = c.lost(err)
		return c.broken
	}
	for {
		var r *proto.Frame
		select {
		case r = <-c.res:
		case <-c.done:
			// The reader may have queued this call's answer just before
			// the stream ended.
			select {
			case r = <-c.res:
			default:
				if ctx.Err() != nil {
					c.broken = fmt.Errorf("%s: %w", op, ctx.Err())
				} else {
					c.broken = c.lost(c.readErr)
				}
				return c.broken
			}
		}
		if r.ID != id && r.ID != 0 {
			continue // an answer to an earlier, abandoned call
		}
		if r.Err != nil {
			if r.ID == 0 {
				c.broken = r.Err
			}
			return r.Err
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(r.OK, result); err != nil {
			return fmt.Errorf("decode %s result: %w", op, err)
		}
		return nil
	}
}

// Close ends the session.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.closed) })
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.sess != nil {
		_ = c.sess.Close()
	}
	if err := c.kill(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// signers collects the keys to offer: ssh-agent's, then the key file's.
func signers(cfg *Config, getenv func(string) string) ([]ssh.Signer, func(), error) {
	var out []ssh.Signer
	closeAgent := func() {}
	if ac, err := dialAgent(getenv); err == nil {
		closeAgent = func() { _ = ac.Close() }
		if ss, err := agent.NewClient(ac).Signers(); err == nil {
			out = append(out, ss...)
		}
	}
	path, err := cfg.KeyPath()
	if err != nil {
		return nil, closeAgent, err
	}
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // the configured key file
		if err != nil {
			closeAgent()
			return nil, func() {}, proto.Errf(proto.CodeAuth, "fix key: in "+ConfigFile+", or remove it to use ssh-agent",
				fmt.Sprintf("cannot read key file: %v", err))
		}
		s, err := ssh.ParsePrivateKey(b)
		var pm *ssh.PassphraseMissingError
		switch {
		case errors.As(err, &pm):
			if len(out) == 0 {
				closeAgent()
				return nil, func() {}, proto.Errf(proto.CodeAuth, "add it to ssh-agent: `ssh-add "+path+"`",
					"key file "+path+" is passphrase-protected and ssh-agent has no keys")
			}
		case err != nil:
			closeAgent()
			return nil, func() {}, proto.Errf(proto.CodeAuth, "point key: at an OpenSSH private key, or remove it to use ssh-agent",
				fmt.Sprintf("cannot parse key file %s: %v", path, err))
		default:
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		closeAgent()
		return nil, func() {}, proto.Errf(proto.CodeAuth, "start ssh-agent and `ssh-add` your key, or set key: in "+ConfigFile,
			"no SSH key available")
	}
	return out, closeAgent, nil
}

// quoteStderr is " (who said: …)" quoting what a program printed on
// stderr, escaped, on one line; or "" when it printed nothing.
func quoteStderr(stderr, who string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = safetext.Line(strings.TrimSuffix(l, "\r"))
	}
	return " (" + who + " said: " + strings.Join(lines, " | ") + ")"
}

// capBuffer keeps the first max bytes written to it, or with tail the
// last max.
type capBuffer struct {
	mu   sync.Mutex
	max  int
	tail bool
	b    []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	switch {
	case b.tail:
		p = p[max(0, len(p)-b.max):]
		b.b = append(b.b, p...)
		b.b = b.b[max(0, len(b.b)-b.max):]
	case len(b.b) < b.max:
		b.b = append(b.b, p[:min(b.max-len(b.b), len(p))]...)
	}
	return n, nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.b)
}
