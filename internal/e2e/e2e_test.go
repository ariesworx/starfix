// Package e2e runs the starfix CLI and MCP server against starfixd through
// a real SSH handshake: an in-process SSH server that authenticates keys,
// maps each to a principal and enforces forced-command semantics by running
// the stdio bridge whatever the client asks for, in front of a daemon on a
// temporary unix socket and a real Dolt store.
//
// A world is one daemon and its SSH server, and a user is a developer with
// a key and a repository whose .starfix.yaml points at them. Each test
// makes its own world, whose cleanup stops both and waits for their
// goroutines. TestMain starts one Dolt server for every test; without dolt
// on PATH the tests skip, unless STARFIX_REQUIRE_DOLT is set.
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/iaptest"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
)

var (
	dolt    *dolttest.Server
	doltErr error
)

func TestMain(m *testing.M) {
	iaptest.Main()
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "starfix-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	dolt, doltErr = dolttest.Start(ctx, dir)
	cancel()
	if dolt != nil {
		defer func() { _ = dolt.Stop() }()
	}
	return m.Run()
}

const project = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"

// world is one daemon behind one SSH server.
type world struct {
	t       *testing.T
	socket  string
	addr    *net.TCPAddr
	hostFpr string
	keysMu  sync.Mutex        // guards keys
	keys    map[string]string // authorized key fingerprint → principal

	// fixedClock says the store runs on the test's clock (daemonOpts.now).
	fixedClock bool
	connsMu    sync.Mutex            // guards conns and dropOp
	conns      map[net.Conn]struct{} // open SSH connections
	// dsn is the store's database, for tests that plant rows the store
	// would refuse (data from before a validation, or a hostile writer).
	dsn string
	// dropOp, when set, names an op whose next response is never
	// delivered: the connection drops after the server has answered
	// (dropReplyTo).
	dropOp string

	// opts are the daemon's options, kept so restartDaemon starts its
	// successor alike; daemonMu guards stopDaemon, which stops the
	// daemon running now.
	opts       daemonOpts
	daemonMu   sync.Mutex
	stopDaemon func()
}

// daemonOpts configures a world's daemon; the zero value takes the
// defaults.
type daemonOpts struct {
	// protoMin and protoMax are the protocol versions the server accepts.
	protoMin, protoMax int
	// noDaemon starts only the SSH server, so the bridge finds no daemon.
	noDaemon bool
	// latest is the newest release the server knows of.
	latest string
	// now, if set, is the store's clock. The reaper and the agents'
	// renewals are then off, so claims expire only when the test says.
	now func() time.Time
	// admins are the store's admins.
	admins []string
	// limits are the daemon's limits, the store's included.
	limits server.Limits
	// reap, if set, is how often the reaper runs, on the store's clock
	// even when now sets it. Unset, it runs every 30s, or never with now.
	reap time.Duration
	// commit, if set, is how often the store makes a Dolt commit; unset,
	// it never does.
	commit time.Duration
	// logger, if set, receives the daemon's and the store's log.
	logger *slog.Logger
}

// newWorld starts a daemon on a new database, unless o.noDaemon, and the
// SSH server in front of it, and stops both when t ends.
func newWorld(t *testing.T, o daemonOpts) *world {
	t.Helper()
	if errors.Is(doltErr, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the end-to-end tests")
	}
	if doltErr != nil {
		t.Fatalf("dolt sql-server: %v", doltErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	base, err := os.MkdirTemp("", "sfe") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	w := &world{t: t, socket: filepath.Join(base, "run", "starfixd.sock"), keys: map[string]string{},
		conns: map[net.Conn]struct{}{}, opts: o}

	if o.now != nil && o.reap == 0 {
		w.fixedClock, w.opts.reap = true, -1
	}
	if o.commit == 0 {
		w.opts.commit = -1
	}
	if !o.noDaemon {
		dsn, err := dolt.NewDatabase(ctx)
		if err != nil {
			t.Fatal(err)
		}
		w.dsn = dsn
		if err := w.startDaemon(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			w.daemonMu.Lock()
			defer w.daemonMu.Unlock()
			w.stopDaemon()
		})
	}
	w.startSSH(ctx)
	return w
}

// startDaemon opens the store on the world's database and serves it on
// the world's socket until stopDaemon is called. The caller holds
// daemonMu, or is newWorld before anything else can run.
func (w *world) startDaemon() error {
	o := w.opts
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, w.dsn, store.Options{Prefix: "sf", CommitInterval: o.commit, Now: o.now, Admins: o.admins,
		Limits: o.limits.Limits, Logger: o.logger})
	if err != nil {
		cancel()
		return err
	}
	srv, err := server.New(server.Config{Store: st, Project: project, Version: "v0.2.0",
		ProtoMin: o.protoMin, ProtoMax: o.protoMax, Latest: o.latest, ReapInterval: o.reap, Limits: o.limits,
		Logger: o.logger})
	if err != nil {
		cancel()
		return errors.Join(err, st.Close())
	}
	l, err := server.Listen(w.socket)
	if err != nil {
		cancel()
		return errors.Join(err, st.Close())
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()
	w.stopDaemon = func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("serve: %v", err)
		}
		if err := st.Close(); err != nil {
			w.t.Errorf("close store: %v", err)
		}
	}
	return nil
}

// restartDaemon stops the daemon, closing every connection to it, and
// starts another on the same database and socket, as an operator's
// restart would.
func (w *world) restartDaemon() error {
	w.daemonMu.Lock()
	defer w.daemonMu.Unlock()
	w.stopDaemon()
	w.stopDaemon = func() {} // until a new daemon starts, nothing runs to stop
	return w.startDaemon()
}

// startSSH runs the in-process sshd. Like OpenSSH with
// restrict,command="starfixd stdio --principal NAME", it maps the key to a
// principal, refuses pty and forwarding, and runs the bridge for any exec
// or shell request, ignoring the command the client sent.
func (w *world) startSSH(ctx context.Context) {
	t := w.t
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	w.hostFpr = ssh.FingerprintSHA256(hostSigner.PublicKey())
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			w.keysMu.Lock()
			defer w.keysMu.Unlock()
			p, ok := w.keys[ssh.FingerprintSHA256(key)]
			if !ok {
				return nil, errors.New("unknown key")
			}
			return &ssh.Permissions{Extensions: map[string]string{"principal": p}}, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w.addr = l.Addr().(*net.TCPAddr)
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { w.serveSSH(ctx, nc, cfg) })
		}
	})
}

// dropReplyTo arranges for the next request of op to be applied by the
// server and its response lost: the SSH connection drops instead of
// delivering it, as a network failure at the worst moment would.
func (w *world) dropReplyTo(op string) {
	w.connsMu.Lock()
	defer w.connsMu.Unlock()
	w.dropOp = op
}

// tap watches one SSH connection's frames for dropReplyTo: reads note the
// id of the request to drop, writes drop the connection at its response.
type tap struct {
	w    *world
	nc   net.Conn
	in   io.Reader
	out  io.Writer
	mu   sync.Mutex // guards drop, rbuf and wbuf
	drop map[uint64]bool
	rbuf []byte // partial request line
	wbuf []byte // partial response line
}

func (t *tap) Read(p []byte) (int, error) {
	n, err := t.in.Read(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rbuf = append(t.rbuf, p[:n]...)
	for {
		i := bytes.IndexByte(t.rbuf, '\n')
		if i < 0 {
			break
		}
		var f proto.Frame
		if json.Unmarshal(t.rbuf[:i], &f) == nil && f.T == proto.FrameReq {
			t.w.connsMu.Lock()
			if f.Op == t.w.dropOp {
				t.w.dropOp = ""
				t.drop[f.ID] = true
			}
			t.w.connsMu.Unlock()
		}
		t.rbuf = t.rbuf[i+1:]
	}
	return n, err
}

func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wbuf = append(t.wbuf, p...)
	for {
		i := bytes.IndexByte(t.wbuf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := t.wbuf[:i+1]
		var f proto.Frame
		if json.Unmarshal(line, &f) == nil && f.T == proto.FrameRes && t.drop[f.ID] {
			_ = t.nc.Close()
			return 0, errors.New("e2e: response dropped")
		}
		if _, err := t.out.Write(line); err != nil {
			return 0, err
		}
		t.wbuf = t.wbuf[i+1:]
	}
}

// dropAll closes every open SSH connection, as a network drop would.
func (w *world) dropAll() {
	w.connsMu.Lock()
	defer w.connsMu.Unlock()
	for c := range w.conns {
		_ = c.Close()
	}
}

// serveSSH serves one SSH connection until it closes, then waits for its
// sessions. A session runs the bridge on its first exec or shell request;
// other channel types and requests are refused.
func (w *world) serveSSH(ctx context.Context, nc net.Conn, cfg *ssh.ServerConfig) {
	w.connsMu.Lock()
	w.conns[nc] = struct{}{}
	w.connsMu.Unlock()
	defer func() {
		w.connsMu.Lock()
		delete(w.conns, nc)
		w.connsMu.Unlock()
		_ = nc.Close()
	}()
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	principal := sc.Permissions.Extensions["principal"]
	var wg sync.WaitGroup
	defer wg.Wait()
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.Prohibited, "restricted")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		wg.Go(func() {
			defer func() { _ = ch.Close() }()
			for req := range creqs {
				switch req.Type {
				case "exec", "shell":
					_ = req.Reply(true, nil)
					status := uint32(0)
					tp := &tap{w: w, nc: nc, in: ch, out: ch, drop: map[uint64]bool{}}
					if err := server.Bridge(ctx, w.socket, principal, tp, tp); err != nil {
						_, _ = fmt.Fprintf(ch.Stderr(), "starfixd: %v\n", err)
						status = 1
					}
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
					return
				default: // pty-req, env, x11, agent forwarding: refused, as with restrict
					_ = req.Reply(false, nil)
				}
			}
		})
	}
}

// user is a developer: a key, and a repository whose .starfix.yaml points at
// the world's server.
type user struct {
	w    *world
	repo string
	env  map[string]string
}

// newUser makes a user with a new key, which the server maps to principal
// ("" leaves the key unknown to it). The user's .starfix.yaml pins the
// server's host key, or hostFpr if it is set.
func (w *world) newUser(principal, hostFpr string) *user {
	t := w.t
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if principal != "" {
		w.keysMu.Lock()
		w.keys[ssh.FingerprintSHA256(signer.PublicKey())] = principal
		w.keysMu.Unlock()
	}
	block, err := ssh.MarshalPrivateKey(priv, principal+"@example.com")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if hostFpr == "" {
		hostFpr = w.hostFpr
	}
	cfg := fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: %d\n  user: starfix\n  host_key: %s\nkey: id_ed25519\n",
		project, w.addr.Port, hostFpr)
	if err := os.WriteFile(filepath.Join(repo, ".starfix.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &user{w: w, repo: repo, env: map[string]string{}}
}

// result is one CLI run's exit code and output.
type result struct {
	code           int
	stdout, stderr string
}

// run invokes the CLI as this user, from a subdirectory of the repository.
func (u *user) run(version string, args ...string) result {
	u.w.t.Helper()
	sub := filepath.Join(u.repo, "src")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		u.w.t.Fatal(err)
	}
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := u.runTo(ctx, &out, &errb, version, args...)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// runTo invokes the CLI as this user until ctx ends, writing to the
// writers given.
func (u *user) runTo(ctx context.Context, stdout, stderr io.Writer, version string, args ...string) int {
	sub := filepath.Join(u.repo, "src")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		u.w.t.Error(err)
		return -1
	}
	return cli.Run(ctx, append([]string{"-C", sub}, args...), cli.Env{
		Stdin: strings.NewReader("from stdin\n"), Stdout: stdout, Stderr: stderr,
		Getenv:   func(k string) string { return u.env[k] }, // no SSH_AUTH_SOCK: the key file is used
		Hostname: func() (string, error) { return "laptop-test", nil },
		Version:  version,
	})
}

// ok runs the CLI as client v0.2.0 and returns its standard output,
// failing the test unless it exits 0.
func (u *user) ok(args ...string) string {
	u.w.t.Helper()
	r := u.run("v0.2.0", args...)
	if r.code != 0 {
		u.w.t.Fatalf("sfx %s: exit %d\n%s%s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// decode unmarshals s as a T, failing the test if it cannot.
func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

func TestCRUDRoundTrip(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")

	a := decode[proto.WriteResult](t, alice.ok("create", "Ship", "the", "thing", "-p", "1", "--label", "area:api", "--json"))
	if a.Rev != 1 || !strings.HasPrefix(a.ID, "sf-") {
		t.Fatalf("create: %+v", a)
	}
	b := strings.TrimSpace(alice.ok("create", "Write the docs", "--body", "-", "-t", "chore"))
	alice.ok("dep", "add", a.ID, b)

	if out := alice.ok("ready"); !strings.Contains(out, b) || strings.Contains(out, a.ID) {
		t.Fatalf("ready:\n%s", out)
	}
	if out := alice.ok("blocked"); !strings.Contains(out, a.ID) || !strings.Contains(out, "blocked by "+b) {
		t.Fatalf("blocked:\n%s", out)
	}
	if got := alice.ok("update", a.ID, "--title", "Ship it", "--assignee", "alice"); got != a.ID+" rev 2\n" {
		t.Fatalf("update: %q", got)
	}
	show := alice.ok("show", a.ID)
	for _, want := range []string{a.ID + "  P1  open  task  rev 2", "Ship it", "assignee: alice", "labels: area:api", "depends on: " + b + " (blocks)"} {
		if !strings.Contains(show, want) {
			t.Fatalf("show lacks %q:\n%s", want, show)
		}
	}
	if s := decode[proto.ShowResult](t, alice.ok("show", b, "--json")); s.Issue.Body != "from stdin" || s.Issue.Type != "chore" {
		t.Fatalf("show --json: %+v", s.Issue)
	}

	alice.ok("label", "add", b, "docs", "small")
	alice.ok("label", "rm", b, "small")
	if l := decode[proto.ListResult](t, alice.ok("list", "--label", "docs", "--json")); len(l.Issues) != 1 || l.Issues[0].ID != b {
		t.Fatalf("list --label: %+v", l)
	}
	cid := strings.TrimSpace(alice.ok("comment", b, "looks", "good"))
	if out := alice.ok("comments", b); !strings.Contains(out, "alice") || !strings.Contains(out, "looks good") {
		t.Fatalf("comments:\n%s", out)
	}
	if c := decode[proto.CommentsResult](t, alice.ok("comments", b, "--json")); len(c.Comments) != 1 || c.Comments[0].ID != cid {
		t.Fatalf("comments --json: %+v", c)
	}

	alice.ok("close", b, "--reason", "done")
	if out := alice.ok("list"); strings.Contains(out, b) {
		t.Fatalf("closed issue in default list:\n%s", out)
	}
	if out := alice.ok("list", "--all"); !strings.Contains(out, b) {
		t.Fatalf("closed issue missing from list --all:\n%s", out)
	}
	if out := alice.ok("ready"); !strings.Contains(out, a.ID) {
		t.Fatalf("unblocked issue not ready:\n%s", out)
	}
	alice.ok("reopen", b)
	alice.ok("dep", "rm", a.ID, b)

	h := decode[proto.HistoryResult](t, alice.ok("history", b, "--json"))
	var ops []string
	for _, e := range h.Events {
		ops = append(ops, e.Op)
		if e.Principal != "alice" || e.Machine != "laptop-test" || e.Session != client.CLISession {
			t.Fatalf("event actor: %+v", e)
		}
	}
	if got := strings.Join(ops, " "); got != "issue.create label.add label.add label.remove comment.add issue.close issue.reopen" {
		t.Fatalf("history: %s", got)
	}
	if out := alice.ok("history", a.ID); !strings.Contains(out, "issue.update") || !strings.Contains(out, "assignee, title") {
		t.Fatalf("history:\n%s", out)
	}
}

func TestSessionFromEnvironment(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	alice.env["CLAUDE_SESSION_ID"] = "claude-123"
	id := strings.TrimSpace(alice.ok("create", "one"))
	alice.env["STARFIX_SESSION"] = "starfix-456"
	alice.ok("comment", id, "two")
	h := decode[proto.HistoryResult](t, alice.ok("history", id, "--json"))
	if len(h.Events) != 2 || h.Events[0].Session != "claude-123" || h.Events[1].Session != "starfix-456" {
		t.Fatalf("sessions: %+v", h.Events)
	}
}

func TestConflict(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := strings.TrimSpace(alice.ok("create", "contested"))
	bob.ok("update", id, "--rev", "1", "-p", "0")

	r := alice.run("v0.2.0", "update", id, "--rev", "1", "--title", "mine")
	want := fmt.Sprintf("sfx: %s changed since rev 1 (now rev 2 by bob)\nfix: re-read with `sfx show %s`\n", id, id)
	if r.code != cli.ExitFailure || r.stderr != want || r.stdout != "" {
		t.Fatalf("exit %d\nstderr %q\nwant   %q", r.code, r.stderr, want)
	}
	r = alice.run("v0.2.0", "update", id, "--rev", "1", "--title", "mine", "--json")
	e := decode[map[string]proto.Error](t, r.stdout)["error"]
	if r.code != cli.ExitFailure || e.Code != proto.CodeConflict || !strings.Contains(e.Message, "now rev 2 by bob") {
		t.Fatalf("json conflict: exit %d %+v", r.code, e)
	}
	// Without --rev the CLI reads the current revision first.
	alice.ok("update", id, "--title", "mine")
}

func TestRefusals(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	other := w.newUser("", "") // a key the server does not know
	pinnedWrong := w.newUser("carol", "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU")

	tests := []struct {
		name string
		u    *user
		args []string
		code int
		want []string
	}{
		{name: "wrong host key", u: pinnedWrong, args: []string{"ready"}, code: cli.ExitFailure,
			want: []string{"host key mismatch for 127.0.0.1", w.hostFpr, "fix: if the server was rebuilt"}},
		{name: "unknown key", u: other, args: []string{"ready"}, code: cli.ExitFailure,
			want: []string{"refused your SSH key", "fix: send your public key"}},
		{name: "not found", u: w.newUser("dave", ""), args: []string{"show", "sf-zzzzzzzz"}, code: cli.ExitFailure,
			want: []string{"sfx: issue sf-zzzzzzzz not found\nfix: find the id with `sfx list`"}},
		{name: "usage", u: other, args: []string{"show"}, code: cli.ExitUsage,
			want: []string{"show needs exactly one issue id", "fix: usage: sfx show ID"}},
		{name: "unknown command", u: other, args: []string{"explode"}, code: cli.ExitUsage,
			want: []string{`unknown command "explode"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.u.run("v0.2.0", tc.args...)
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", r.code, tc.code, r.stderr)
			}
			for _, s := range tc.want {
				if !strings.Contains(r.stderr, s) {
					t.Fatalf("stderr lacks %q:\n%s", s, r.stderr)
				}
			}
		})
	}
}

func TestProtocolRangeRefusal(t *testing.T) {
	w := newWorld(t, daemonOpts{protoMin: 4, protoMax: 5})
	alice := w.newUser("alice", "")
	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitVersion {
		t.Fatalf("exit %d, want %d\n%s", r.code, cli.ExitVersion, r.stderr)
	}
	want := "sfx: starfix speaks protocol 3; the server (v0.2.0) needs 4 to 5\nfix: upgrade this client: `sfx upgrade`\n"
	if r.stderr != want {
		t.Fatalf("stderr %q\nwant   %q", r.stderr, want)
	}
	r = alice.run("v0.2.0", "--json", "ready")
	if e := decode[map[string]proto.Error](t, r.stdout)["error"]; r.code != cli.ExitVersion || e.Code != proto.CodeVersion {
		t.Fatalf("json: exit %d %+v", r.code, e)
	}
}

func TestOlderClientWarns(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	r := alice.run("v0.1.0", "ready")
	if r.code != 0 || r.stderr != "sfx: starfix v0.1.0 is older than the server (v0.2.0); run `sfx upgrade`\n" {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	if r := alice.run("v0.2.0", "ready"); r.stderr != "" {
		t.Fatalf("same version warned: %q", r.stderr)
	}
}

// A restart whose new daemon fails to start leaves nothing to stop: the
// world's cleanup must not wait on the daemon the restart already
// stopped.
func TestRestartDaemonFails(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	// The listener refuses a socket directory others can read.
	if err := os.Chmod(filepath.Dir(w.socket), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.restartDaemon(); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("restartDaemon with an open socket directory = %v, want its refusal", err)
	}
	stop, stopped := w.stopDaemon, make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		w.stopDaemon = func() {} // let the cleanup finish; the goroutine above stays blocked
		t.Fatal("stopDaemon after a failed restart did not return: it waits again on the daemon the restart stopped")
	}
}

func TestDaemonDown(t *testing.T) {
	w := newWorld(t, daemonOpts{noDaemon: true})
	r := w.newUser("alice", "").run("v0.2.0", "ready")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "starfixd is not running on the server") ||
		!strings.Contains(r.stderr, "fix: ask the server admin to start it") {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
}
