// Package server is starfixd: the daemon that owns the store and serves the
// protocol on a unix socket, and the stdio bridge that sshd runs as each
// developer key's forced command.
//
// # Trust
//
// The daemon never authenticates anyone itself. sshd authenticates the
// developer's key, and that key's authorized_keys line forces
//
//	restrict,command="starfixd stdio --principal NAME" ssh-ed25519 AAAA…
//
// so the principal is fixed by which key logged in; whatever command the
// client asks for is ignored. The bridge connects to the daemon's socket
// and sends a bridge frame naming that principal before any client byte.
// The daemon believes the bridge frame only because the socket peer is
// local and runs as the daemon's own Unix user: the socket lives in a 0700
// directory, and on Linux each connection's SO_PEERCRED uid must equal the
// daemon's. The SSH login account (server.user) is therefore the account
// starfixd runs as, and its authorized_keys holds only restricted lines.
// A bridge frame anywhere but first is refused.
package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
	"github.com/ariesworx/starfix/internal/store"
)

// Config configures a Server.
type Config struct {
	// Store is the issue store. Required.
	Store *store.Store
	// Project is the UUID this daemon serves; a hello naming another is
	// refused. Required.
	Project string
	// Version is the server's build version, reported in the handshake.
	Version string
	// ProtoMin and ProtoMax bound the client protocol versions accepted.
	// Zero takes proto.ProtoMin and proto.ProtoMax.
	ProtoMin, ProtoMax int
	// Latest is the latest release the server knows of; may be empty.
	Latest string
	// Logger records one line per request. Default discards.
	Logger *slog.Logger
	// PeerCheck vets each connection before its bridge frame is trusted.
	// Nil uses CheckPeer.
	PeerCheck func(net.Conn) error
	// RequestTimeout bounds one request. Default 60s.
	RequestTimeout time.Duration
	// ReapInterval is how often expired claims are ended. Default 30s;
	// negative turns the reaper off (tests call Reap and Prune).
	ReapInterval time.Duration
	// Limits bound connections, idling, writes and refusal logging per
	// principal; zero fields take DefaultLimits. Its store limits are the
	// store's to apply (store.Options.Limits), and only shown here.
	Limits Limits
}

// Server serves the protocol. Create it with New.
type Server struct {
	cfg Config

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup

	// slots counts connections past the handshake (Limits.Conns and
	// ConnsPerPrincipal); writes is each principal's write bucket;
	// refusals samples refusal log lines.
	slots    conns
	writes   buckets
	refusals sampler
	// now is the clock for the limits above; tests replace it.
	now func() time.Time
}

var (
	// PrincipalPattern is what a principal name may look like; the store
	// holds the one definition, which mentions and handoffs also use.
	PrincipalPattern = store.PrincipalPattern
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// validActorPart reports whether a session or machine name is 1-255 bytes
// on one line, without control or bidirectional characters (safetext), as
// the store requires of every actor.
func validActorPart(s string) bool { return len(s) <= 255 && s != "" && safetext.ValidLine(s) }

// New checks cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("server: no store")
	}
	if !uuidPattern.MatchString(cfg.Project) {
		return nil, fmt.Errorf("server: project %q is not a lowercase UUID", cfg.Project)
	}
	if cfg.ProtoMin == 0 {
		cfg.ProtoMin = proto.ProtoMin
	}
	if cfg.ProtoMax == 0 {
		cfg.ProtoMax = proto.ProtoMax
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.PeerCheck == nil {
		cfg.PeerCheck = CheckPeer
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 60 * time.Second
	}
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = 30 * time.Second
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	cfg.Limits = cfg.Limits.WithDefaults()
	s := &Server{cfg: cfg, conns: map[net.Conn]struct{}{}, now: time.Now}
	s.writes = buckets{rate: cfg.Limits.WriteRate, burst: float64(cfg.Limits.WriteBurst)}
	s.refusals = sampler{n: cfg.Limits.RefusalLogs}
	return s, nil
}

// Serve accepts connections on l until ctx is done, then closes l and every
// open connection and waits for their handlers to return.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() {
		_ = l.Close()
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
	})
	defer stop()
	defer s.wg.Wait()
	if s.cfg.ReapInterval > 0 {
		s.wg.Go(func() { s.reapLoop(ctx) })
	}
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.mu.Lock()
		full := len(s.conns) >= 2*s.cfg.Limits.Conns
		if !full {
			s.conns[c] = struct{}{}
		}
		s.mu.Unlock()
		if full {
			// Past twice the cap, sockets still in the handshake are
			// closed unread; the cap itself is refused cleanly there.
			s.refusals.log(s.cfg.Logger, "", s.now(), slog.LevelWarn, "refused connection", "reason", "connection limit")
			_ = c.Close()
			continue
		}
		s.wg.Go(func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.handle(ctx, c)
		})
	}
}

// pruneEvery is how often the reaper also prunes the registry and the
// inbox.
const pruneEvery = 10 * time.Minute

// reapLoop ends expired claims every ReapInterval until ctx is done, and
// prunes every pruneEvery.
func (s *Server) reapLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	var pruned time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reap(ctx)
			if time.Since(pruned) >= pruneEvery {
				s.Prune(ctx)
				pruned = time.Now()
			}
		}
	}
}

// Prune deletes registry rows and read inbox items older than the limits
// keep them (S-6, S-7), and logs what it removed.
func (s *Server) Prune(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	n, err := s.cfg.Store.Prune(pctx, time.Duration(s.cfg.Limits.AgentKeep), time.Duration(s.cfg.Limits.InboxKeep))
	if err != nil {
		if ctx.Err() == nil {
			s.cfg.Logger.Error("prune", "err", err)
		}
		return
	}
	if n != (store.Pruned{}) {
		s.cfg.Logger.Info("pruned", "agents", n.Agents, "inbox", n.Inbox)
	}
}

// Reap ends expired claims now and logs each.
func (s *Server) Reap(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	cs, err := s.cfg.Store.ReapClaims(rctx)
	if err != nil {
		if ctx.Err() == nil {
			s.cfg.Logger.Error("reap claims", "err", err)
		}
		return
	}
	for _, c := range cs {
		s.cfg.Logger.Info("claim expired", "issue", c.Issue, "principal", c.Holder.Principal,
			"session", c.Holder.Session, "epoch", c.Epoch)
	}
}

// Handshake timeouts: the bridge frame comes at once; the hello follows the
// SSH session start.
const (
	bridgeTimeout = 5 * time.Second
	helloTimeout  = 30 * time.Second
)

type session struct {
	actor store.Actor
	// harness is what the hello said the client runs under, unchecked.
	harness string
	enc     *proto.Encoder
	dec     *proto.Decoder

	// push is the running watch, if the client sent watch. Only the
	// request loop sets it; pushMu orders pushed frames against the
	// loop's flush before each response.
	push   *pusher
	pushMu sync.Mutex
	// slot is set when the session holds a connection slot (s.slots).
	slot bool
}

// watching reports whether sess's watch is running, which exempts it
// from the idle timeout.
func (sess *session) watching() bool {
	p := sess.push
	if p == nil {
		return false
	}
	sess.pushMu.Lock()
	defer sess.pushMu.Unlock()
	return !p.ended
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	log := s.cfg.Logger
	if err := s.cfg.PeerCheck(c); err != nil {
		s.refusals.log(log, "", s.now(), slog.LevelWarn, "refused socket peer", "err", err)
		return
	}
	sess, err := s.handshake(c)
	if sess != nil && sess.slot {
		defer s.slots.release(sess.actor.Principal)
	}
	if err != nil {
		key := ""
		if sess != nil {
			key = sess.actor.Principal
		}
		s.refusals.log(log, key, s.now(), slog.LevelInfo, "handshake failed", "err", err)
		return
	}
	principal := sess.actor.Principal
	log = log.With("principal", principal, "session", sess.actor.Session, "machine", sess.actor.Machine)
	log.Debug("connected")
	defer func() {
		_ = c.Close() // unblocks a pusher stuck writing to a client that stopped reading
		s.unwatch(sess)
	}()
	s.touch(ctx, log, sess.actor, sess.harness)
	idle := time.Duration(s.cfg.Limits.IdleTimeout)
	for {
		if sess.watching() {
			_ = c.SetReadDeadline(time.Time{})
		} else {
			_ = c.SetReadDeadline(s.now().Add(idle))
		}
		f, err := sess.dec.Decode()
		if err != nil {
			var ne net.Error
			switch {
			case errors.Is(err, io.EOF) || ctx.Err() != nil:
			case errors.As(err, &ne) && ne.Timeout():
				log.Debug("idle connection closed", "after", idle)
				_ = s.send(sess, 0, nil, proto.Errf(proto.CodeUnavailable, "reconnect; starfix does so on the next command",
					fmt.Sprintf("connection closed after %s idle", proto.Span(idle))))
			default:
				s.refusals.log(log, principal, s.now(), slog.LevelInfo, "read failed", "err", err)
				if errors.Is(err, proto.ErrFrameTooLarge) {
					_ = s.send(sess, 0, nil, proto.Errf(proto.CodeInvalid, "send large text in smaller parts",
						"request larger than 4 MiB"))
				}
			}
			return
		}
		if f.T != proto.FrameReq {
			_ = s.send(sess, f.ID, nil, proto.Errf(proto.CodeInvalid, "upgrade starfix to match the server",
				fmt.Sprintf("unexpected %q frame after the handshake", f.T)))
			return
		}
		start := time.Now()
		rctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		var res any
		var perr *proto.Error
		ok, wait := true, time.Duration(0)
		if !readOps[f.Op] {
			ok, wait = s.writes.take(principal, s.now())
		}
		switch {
		case !ok:
			perr = s.busyWrites(principal, wait)
		case f.Op == proto.OpWatch:
			res, perr = s.watch(rctx, sess, f.Args)
		default:
			res, perr = s.Dispatch(rctx, sess.actor, f.Op, f.Args)
		}
		cancel()
		if p := sess.push; p != nil {
			s.flush(sess, p) // what was committed before this answer goes first
		}
		attrs := []any{"op", f.Op, "ms", time.Since(start).Milliseconds()}
		if perr != nil {
			// A refusal is worth seeing at the default level, sampled per
			// principal (S-20); a success is one line per request, so only
			// at debug.
			s.refusals.log(log, principal, s.now(), slog.LevelInfo, "request", append(attrs, "code", string(perr.Code))...)
		} else {
			log.Debug("request", attrs...)
		}
		if err := s.send(sess, f.ID, res, perr); err != nil {
			log.Info("write failed", "err", err)
			return
		}
	}
}

func (s *Server) send(sess *session, id uint64, res any, perr *proto.Error) error {
	f, err := proto.Response(id, res, perr)
	if err != nil {
		f, _ = proto.Response(id, nil, proto.Errf(proto.CodeUnavailable, "retry; if it persists, report it", "server could not encode the result"))
	}
	err = sess.enc.Encode(f)
	if errors.Is(err, proto.ErrFrameTooLarge) {
		// Paged reads keep replies well under the limit; should one still
		// pass it, refuse the request rather than drop the connection.
		f, _ = proto.Response(id, nil, proto.Errf(proto.CodeInvalid, "ask for less: a smaller --limit, or page with --before",
			"the result is larger than 4 MiB"))
		err = sess.enc.Encode(f)
	}
	return err
}

// busyWrites refuses a write over principal's rate, saying when to retry.
func (s *Server) busyWrites(principal string, wait time.Duration) *proto.Error {
	l := s.cfg.Limits
	return proto.Errf(proto.CodeBusy, fmt.Sprintf("wait %s and retry", wait),
		fmt.Sprintf("%s is over the write limit (%g a second, bursts of %d)", principal, l.WriteRate, l.WriteBurst))
}

// handshake reads the bridge frame and the client's hello, and answers with
// a welcome, or a refusal and an error.
func (s *Server) handshake(c net.Conn) (*session, error) {
	sess := &session{enc: proto.NewEncoder(c), dec: proto.NewDecoder(c)}
	_ = c.SetReadDeadline(time.Now().Add(bridgeTimeout))
	f, err := sess.dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("bridge frame: %w", err)
	}
	if f.T != proto.FrameBridge || !PrincipalPattern.MatchString(f.Principal) || store.Reserved(f.Principal) {
		return nil, fmt.Errorf("first frame is %q, not a bridge frame with a valid, unreserved principal", f.T)
	}
	sess.actor.Principal = f.Principal

	_ = c.SetReadDeadline(time.Now().Add(helloTimeout))
	f, err = sess.dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("hello: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	w := &proto.Frame{T: proto.FrameWelcome, Version: s.cfg.Version, Min: s.cfg.ProtoMin, Max: s.cfg.ProtoMax, Latest: s.cfg.Latest}
	// A refusal past the bridge frame returns sess, so the caller knows
	// whose it is.
	refuse := func(e *proto.Error) (*session, error) {
		w.Err = e
		return sess, errors.Join(e, sess.enc.Encode(w))
	}
	if f.T != proto.FrameHello {
		return refuse(proto.Errf(proto.CodeInvalid, "upgrade starfix to match the server",
			fmt.Sprintf("expected a hello, got a %q frame", f.T)))
	}
	if e := proto.CheckProto(f.Proto, s.cfg.ProtoMin, s.cfg.ProtoMax, s.cfg.Version); e != nil {
		return refuse(e)
	}
	if f.Project != s.cfg.Project {
		return refuse(proto.Errf(proto.CodeNotFound, "check `project` in .starfix.yaml, or the server address",
			fmt.Sprintf("project %q is not served by this server", f.Project)))
	}
	sess.actor.Session = f.Session
	if sess.actor.Session == "" {
		if sess.actor.Session, err = newSessionID(); err != nil {
			return refuse(proto.Errf(proto.CodeUnavailable, "retry", "server could not create a session id"))
		}
	}
	sess.actor.Machine = f.Machine
	if sess.actor.Machine == "" {
		sess.actor.Machine = "unknown"
	}
	if !validActorPart(sess.actor.Session) || !validActorPart(sess.actor.Machine) {
		return refuse(proto.Errf(proto.CodeInvalid, "set STARFIX_SESSION to 1-255 printable characters",
			"session and machine must be 1-255 printable characters, without control or bidirectional characters"))
	}
	sess.harness = f.Harness
	if ok, why := s.slots.acquire(sess.actor.Principal, s.cfg.Limits.Conns, s.cfg.Limits.ConnsPerPrincipal); !ok {
		return refuse(proto.Errf(proto.CodeBusy, "close other starfix sessions, or wait for them to end, and retry", why))
	}
	sess.slot = true
	w.Session = sess.actor.Session
	w.Principal = sess.actor.Principal
	if err := sess.enc.Encode(w); err != nil {
		return nil, err
	}
	return sess, nil
}

var sessionEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newSessionID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	return "s-" + sessionEncoding.EncodeToString(b[:]), nil
}
