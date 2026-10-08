package board

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Conn is the connection Live reads through: a *client.Conn, or a test's
// fake.
type Conn interface {
	Call(ctx context.Context, op string, args, result any) error
	Done() <-chan struct{}
	Err() error
	Close() error
}

// Dialer opens a connection whose pushed events go to onPush.
type Dialer func(ctx context.Context, onPush func(proto.Push)) (Conn, error)

// Timing. A burst of events is read once, debounce after its first. A
// lost connection is redialed after a random wait up to a ceiling that
// starts at retryFirst and doubles to retryLongest; it starts over only
// once a new connection has read the lists, so a server that accepts
// and then drops every connection is not dialed once a second.
const (
	debounce     = 250 * time.Millisecond
	retryFirst   = time.Second
	retryLongest = 30 * time.Second
)

// What one refresh reads. Ready and blocked list the first listLimit;
// held issues are every live claim up to heldLimit, the server's most.
const (
	listLimit = 100
	heldLimit = 500
)

// pushQueue is how many pushed events Live holds while it is busy. Past
// it they are dropped and the lists read again, since the tail missed
// some.
const pushQueue = 256

// Live keeps a board current: it watches the server's events, sends each
// to the board, and reads the lists again when an event may change them,
// once per burst; it rereads after a resync, and redials a lost
// connection. It never polls. Make one with NewLive.
type Live struct {
	dial   Dialer
	pushes chan proto.Push
	// lost holds a signal that pushes were dropped.
	lost chan struct{}
	// jitter picks a redial wait up to its ceiling.
	jitter func(ceiling time.Duration) time.Duration
}

// NewLive returns a Live that connects with dial.
func NewLive(dial Dialer) *Live {
	return &Live{
		dial:   dial,
		pushes: make(chan proto.Push, pushQueue),
		lost:   make(chan struct{}, 1),
		jitter: fullJitter,
	}
}

// fullJitter spreads redials over the whole of [0, ceiling), so boards
// that lost one server together do not all dial it again at once.
func fullJitter(ceiling time.Duration) time.Duration {
	return rand.N(ceiling) //nolint:gosec // spreading retries needs no secrecy
}

// Connect opens a connection for Run, with its pushes going to l. Call it
// for the first connection, so a refusal (a bad key, an old server) is
// reported before the board takes the terminal.
func (l *Live) Connect(ctx context.Context) (Conn, error) { return l.dial(ctx, l.push) }

// push is the connection's OnPush: it runs on the connection's reader,
// so it never blocks.
func (l *Live) push(p proto.Push) {
	select {
	case l.pushes <- p:
	default:
		select {
		case l.lost <- struct{}{}:
		default:
		}
	}
}

// Run feeds the board on updates through conn, and the connections after
// it, until ctx ends; actions are the board's requests (DoRefresh, DoShow,
// DoHide). It closes every connection it uses. Sends on updates wait, so
// the caller reads them until Run returns.
func (l *Live) Run(ctx context.Context, conn Conn, actions <-chan Action, updates chan<- Update) {
	r := &runner{l: l, ctx: ctx, actions: actions, updates: updates}
	ceiling := time.Duration(0)
	for {
		r.fresh = false
		err := r.serve(conn)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
		if r.fresh {
			ceiling = 0
		}
		r.send(Status{State: StateReconnecting, Note: note(err)})
		for {
			ceiling = min(max(2*ceiling, retryFirst), retryLongest)
			if !r.sleep(l.jitter(ceiling)) {
				return
			}
			c, err := l.dial(ctx, l.push)
			if err == nil {
				conn = c
				break
			}
			r.send(Status{State: StateReconnecting, Note: note(err)})
		}
	}
}

// runner is one Run's state.
type runner struct {
	l       *Live
	ctx     context.Context // Run's; the runner lives only as long as the call
	actions <-chan Action
	updates chan<- Update
	// open is the issue whose detail the board shows, or "".
	open string
	// status is the last Status sent.
	status Status
	// unwatched is why the connection pushes no events, when the server
	// refused the watch: every status says so while it lasts.
	unwatched string
	// fresh is set once the connection being served has read the lists.
	fresh bool
}

// send hands u to the board, unless Run is ending. A Status is sent only
// when it changes.
func (r *runner) send(u Update) {
	if s, ok := u.(Status); ok {
		if s == r.status {
			return
		}
		r.status = s
	}
	select {
	case r.updates <- u:
	case <-r.ctx.Done():
	}
}

// sleep waits d, keeping up with the board's actions meanwhile. It
// reports false when Run is ending.
func (r *runner) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return false
		case <-t.C:
			return true
		case a := <-r.actions:
			r.note(a) // the reconnect reads it
		}
	}
}

// note records what an action changes about the open detail.
func (r *runner) note(a Action) {
	switch a.Do {
	case DoShow:
		r.open = a.ID
	case DoHide:
		r.open = ""
	}
}

// serve watches on conn and keeps the board current until conn is lost
// or Run ends, returning why it stopped (nil when Run ends).
func (r *runner) serve(conn Conn) error {
	if err := r.watch(conn); err != nil {
		return err
	}
	if err := r.refresh(conn, true); err != nil {
		return err
	}
	// fire ends a burst of events; lists and detail say what it changed.
	var fire <-chan time.Time
	lists, detail := false, false
	for {
		select {
		case <-r.ctx.Done():
			return nil
		case <-conn.Done():
			return conn.Err()
		case <-r.l.lost:
			if err := r.refresh(conn, false); err != nil {
				return err
			}
		case p := <-r.l.pushes:
			switch {
			case p.Op == proto.EvResync:
				if err := r.watch(conn); err != nil {
					return err
				}
				if err := r.refresh(conn, true); err != nil {
					return err
				}
				continue
			case p.Op != proto.EvEvent || p.Event == nil:
				continue // an inbox item: not the board's
			}
			r.send(Pushed{*p.Event})
			lists = lists || Relevant(p.Event.Op)
			detail = detail || r.open != "" && p.Event.Issue == r.open
			if fire == nil && (lists || detail) {
				fire = time.After(debounce)
			}
		case <-fire:
			fire = nil
			var err error
			switch {
			case lists:
				err = r.refresh(conn, detail)
			case detail:
				err = r.show(conn)
			}
			if err != nil {
				return err
			}
			lists, detail = false, false
		case a := <-r.actions:
			r.note(a)
			switch a.Do {
			case DoRefresh:
				if err := r.refresh(conn, true); err != nil {
					return err
				}
			case DoShow:
				if err := r.show(conn); err != nil {
					return err
				}
			}
		}
	}
}

// watch asks conn for every event, before anything is read, so nothing
// falls between the read and the pushes.
func (r *runner) watch(conn Conn) error {
	err := conn.Call(r.ctx, proto.OpWatch, proto.WatchArgs{Events: true}, nil)
	r.unwatched = ""
	if err != nil && conn.Err() == nil {
		// Refused: the board can read this server but not follow it.
		r.unwatched = note(err)
		r.send(Status{State: StateLive, Note: r.unwatched})
		return nil
	}
	return err
}

// refresh reads the lists, and the open detail when detail is set, and
// sends them. A refusal is shown and leaves the connection in use; a
// lost connection is returned.
func (r *runner) refresh(conn Conn, detail bool) error {
	snap, err := read(r.ctx, conn)
	switch {
	case err == nil:
		r.fresh = true
		r.send(snap)
		r.send(Status{State: StateLive, Note: r.unwatched})
	case conn.Err() != nil || r.ctx.Err() != nil:
		return err
	default:
		r.send(Status{State: StateLive, Note: note(err)})
	}
	if detail && r.open != "" {
		return r.show(conn)
	}
	return nil
}

// show reads the open issue and sends it, or the refusal in its place.
func (r *runner) show(conn Conn) error {
	if r.open == "" {
		return nil
	}
	var res proto.ShowResult
	err := conn.Call(r.ctx, proto.OpShow, proto.ShowArgs{ID: r.open}, &res)
	switch {
	case err == nil:
		r.send(&Detail{ID: r.open, Show: &res})
	case conn.Err() != nil || r.ctx.Err() != nil:
		return err
	default:
		r.send(&Detail{ID: r.open, Err: note(err)})
	}
	return nil
}

// read reads one snapshot: ready, blocked, the live claims and the titles
// of the issues in progress.
func read(ctx context.Context, conn Conn) (*Snapshot, error) {
	var ready, inProgress proto.ListResult
	var blocked proto.BlockedResult
	var claims proto.ClaimsResult
	if err := conn.Call(ctx, proto.OpReady, proto.LimitArgs{Limit: listLimit}, &ready); err != nil {
		return nil, err
	}
	if err := conn.Call(ctx, proto.OpBlocked, proto.LimitArgs{Limit: listLimit}, &blocked); err != nil {
		return nil, err
	}
	local := time.Now()
	if err := conn.Call(ctx, proto.OpClaims, proto.LimitArgs{Limit: heldLimit}, &claims); err != nil {
		return nil, err
	}
	if err := conn.Call(ctx, proto.OpList, proto.ListArgs{Status: []string{"in_progress"}, Limit: heldLimit}, &inProgress); err != nil {
		return nil, err
	}
	byID := make(map[string]proto.Summary, len(inProgress.Issues))
	for _, x := range inProgress.Issues {
		byID[x.ID] = x
	}
	snap := &Snapshot{Ready: ready.Issues, Blocked: blocked.Issues, HeldMore: claims.More,
		ServerNow: claims.Now, LocalNow: local}
	if snap.ServerNow.IsZero() {
		snap.ServerNow = local
	}
	for _, c := range claims.Claims {
		x := byID[c.ID]
		snap.Held = append(snap.Held, Held{Claim: c, Title: x.Title, Priority: x.Priority})
	}
	return snap, nil
}

// note is err as one line for the board: a refusal's message and fix.
func note(err error) string {
	if err == nil {
		return ""
	}
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		if pe.Fix != "" {
			return pe.Message + "; fix: " + pe.Fix
		}
		return pe.Message
	}
	return err.Error()
}
