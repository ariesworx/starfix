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
	// negative turns the reaper off (tests call Reap).
	ReapInterval time.Duration
}

// Server serves the protocol. Create it with New.
type Server struct {
	cfg Config

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

var (
	// PrincipalPattern is what a principal name may look like.
	PrincipalPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	actorPart        = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,255}$`)
)

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
	return &Server{cfg: cfg, conns: map[net.Conn]struct{}{}}, nil
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
		s.conns[c] = struct{}{}
		s.mu.Unlock()
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

// reapLoop ends expired claims every ReapInterval until ctx is done.
func (s *Server) reapLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reap(ctx)
		}
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
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	log := s.cfg.Logger
	if err := s.cfg.PeerCheck(c); err != nil {
		log.Warn("refused socket peer", "err", err)
		return
	}
	sess, err := s.handshake(c)
	if err != nil {
		log.Info("handshake failed", "err", err)
		return
	}
	log = log.With("principal", sess.actor.Principal, "session", sess.actor.Session, "machine", sess.actor.Machine)
	log.Debug("connected")
	s.touch(ctx, log, sess.actor, sess.harness)
	for {
		f, err := sess.dec.Decode()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				log.Info("read failed", "err", err)
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
		res, perr := s.Dispatch(rctx, sess.actor, f.Op, f.Args)
		cancel()
		attrs := []any{"op", f.Op, "ms", time.Since(start).Milliseconds()}
		if perr != nil {
			// A refusal is worth seeing at the default level; a success is
			// one line per request, so only at debug.
			log.Info("request", append(attrs, "code", string(perr.Code))...)
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
	return sess.enc.Encode(f)
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
	if f.T != proto.FrameBridge || !PrincipalPattern.MatchString(f.Principal) {
		return nil, fmt.Errorf("first frame is %q, not a bridge frame with a valid principal", f.T)
	}
	sess.actor.Principal = f.Principal

	_ = c.SetReadDeadline(time.Now().Add(helloTimeout))
	f, err = sess.dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("hello: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	w := &proto.Frame{T: proto.FrameWelcome, Version: s.cfg.Version, Min: s.cfg.ProtoMin, Max: s.cfg.ProtoMax, Latest: s.cfg.Latest}
	refuse := func(e *proto.Error) (*session, error) {
		w.Err = e
		return nil, errors.Join(e, sess.enc.Encode(w))
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
	if !actorPart.MatchString(sess.actor.Session) || !actorPart.MatchString(sess.actor.Machine) {
		return refuse(proto.Errf(proto.CodeInvalid, "set STARFIX_SESSION to 1-255 printable characters",
			"session and machine must be 1-255 printable characters"))
	}
	sess.harness = f.Harness
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
