package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// The soak's sessions. Each is one agent at work: a libSession drives the
// client library directly, as `sfx` does, and an mcpSession drives `sfx
// mcp`'s tools over an in-memory MCP transport, as an agent harness does.
// Both reach the daemon through the world's SSH server, and every
// protocol request either makes goes through soak.do, which times it,
// journals it and hands it to the monitor's continuous checks.

// sessKey names a session: its principal and the session id it sent.
type sessKey struct{ principal, session string }

func (k sessKey) String() string { return k.principal + "/" + k.session }

// outcome is how one request ended.
type outcome int

const (
	acked   outcome = iota // the server answered with a result
	refused                // the server answered with an error
	lost                   // the connection failed: it may or may not have been applied
)

func (o outcome) String() string { return [...]string{"acked", "refused", "lost"}[o] }

// call is one protocol request, as the client saw it.
type call struct {
	who  sessKey
	op   string
	args any          // the *Args value sent
	res  any          // the *Result value received, when acked
	err  *proto.Error // the refusal, when refused
	out  outcome
	// sent and recv are on the store's clock; took is real time.
	sent, recv time.Time
	took       time.Duration
}

// callTimeout bounds one request. Nothing the soak sends should come near
// it, so reaching it is reported as a hang.
const callTimeout = 60 * time.Second

// window is one watch: pushes are owed to it from start, the store-clock
// time its watch was acknowledged, until its connection breaks or the
// server sends a resync. A watch that asked for events is also owed every
// issue event committed after start.
type window struct {
	key    sessKey
	conn   int
	start  time.Time
	resync bool
	broken bool
	events bool
	got    map[int64]proto.Event // issue events pushed to it, by seq
}

// inboxLog is what one session was pushed, across all its connections.
type inboxLog struct {
	mu      sync.Mutex
	got     map[int64]proto.InboxItem
	windows []*window
}

// line routes one connection's pushes to its current window.
type line struct {
	log  *inboxLog
	key  sessKey
	conn int
	cur  *window // guarded by log.mu
	mon  *monitor
}

func (l *inboxLog) newLine(key sessKey, conn int, mon *monitor) *line {
	return &line{log: l, key: key, conn: conn, mon: mon}
}

// push records a pushed event. It runs on the client's reader goroutine.
func (ln *line) push(p proto.Push) {
	ln.log.mu.Lock()
	defer ln.log.mu.Unlock()
	switch {
	case p.Op == proto.EvResync:
		if ln.cur != nil {
			ln.cur.resync = true
		}
	case p.Item != nil:
		if ln.log.got == nil {
			ln.log.got = map[int64]proto.InboxItem{}
		}
		ln.log.got[p.Item.ID] = *p.Item
		if ln.cur == nil {
			ln.mon.fail("", "%s was pushed inbox item %d on connection %d before any watch", ln.key, p.Item.ID, ln.conn)
		}
		if p.Item.Session != "" && p.Item.Session != ln.key.session {
			ln.mon.fail(p.Item.Issue, "%s was pushed item %d addressed to session %s", ln.key, p.Item.ID, p.Item.Session)
		}
	case p.Event != nil:
		w := ln.cur
		switch {
		case w == nil || !w.events:
			ln.mon.fail(p.Event.Issue, "%s was pushed event %d on connection %d, which did not watch for events", ln.key, p.Event.Seq, ln.conn)
		case w.resync:
			ln.mon.fail(p.Event.Issue, "%s was pushed event %d on connection %d after a resync", ln.key, p.Event.Seq, ln.conn)
		default:
			if _, dup := w.got[p.Event.Seq]; dup {
				ln.mon.fail(p.Event.Issue, "%s was pushed event %d twice on connection %d", ln.key, p.Event.Seq, ln.conn)
			}
			w.got[p.Event.Seq] = *p.Event
		}
	}
}

// watching opens a window as a watch is sent: the server may push before
// it answers, so pushes are accepted from now, and owed once the answer
// arrives. events says the watch asks for issue events.
func (ln *line) watching(events bool) {
	ln.log.mu.Lock()
	defer ln.log.mu.Unlock()
	switch w := ln.cur; {
	case w != nil && !w.start.IsZero() && !w.resync:
		return // watching again while watched changes nothing
	case w != nil && w.start.IsZero():
		w.events = w.events || events // an unanswered watch, sent again
		return
	}
	ln.cur = &window{key: ln.key, conn: ln.conn, events: events, got: map[int64]proto.Event{}}
	ln.log.windows = append(ln.log.windows, ln.cur)
}

// watched starts what the window opened by watching is owed: from start,
// the time the watch was acknowledged.
func (ln *line) watched(start time.Time) {
	ln.log.mu.Lock()
	defer ln.log.mu.Unlock()
	if w := ln.cur; w != nil && w.start.IsZero() {
		w.start = start
	}
}

// broke marks the line's window as ended by a failed connection.
func (ln *line) broke() {
	ln.log.mu.Lock()
	defer ln.log.mu.Unlock()
	if ln.cur != nil {
		ln.cur.broken = true
	}
}

// live returns the line's window, if it was acknowledged and has neither
// broken nor been resynced.
func (ln *line) live() *window {
	ln.log.mu.Lock()
	defer ln.log.mu.Unlock()
	if w := ln.cur; w != nil && !w.start.IsZero() && !w.broken && !w.resync {
		return w
	}
	return nil
}

// delivered reports which of ids were pushed to the session, and whether
// w has seen a resync since.
func (l *inboxLog) delivered(w *window, ids []int64) (missing []int64, resync bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if _, ok := l.got[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing, w.resync
}

// eventsDelivered reports which of seqs were not pushed to w, and whether
// w has seen a resync since.
func (l *inboxLog) eventsDelivered(w *window, seqs []int64) (missing []int64, resync bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, seq := range seqs {
		if _, ok := w.got[seq]; !ok {
			missing = append(missing, seq)
		}
	}
	return missing, w.resync
}

// events returns a copy of the issue events pushed to each of the
// session's windows.
func (l *inboxLog) events() []map[int64]proto.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[int64]proto.Event
	for _, w := range l.windows {
		out = append(out, maps.Clone(w.got))
	}
	return out
}

// resynced reports whether the line's watch was resynced, so that the
// session should watch again.
func (ln *line) resynced() bool {
	ln.log.mu.Lock()
	defer ln.log.mu.Unlock()
	return ln.cur != nil && ln.cur.resync
}

// pushed returns a copy of everything pushed to the session.
func (l *inboxLog) pushed() map[int64]proto.InboxItem {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[int64]proto.InboxItem, len(l.got))
	for k, v := range l.got {
		out[k] = v
	}
	return out
}

// session is what both kinds of session share.
type session struct {
	s     *soak
	key   sessKey
	tag   string // the session's part of every marker and idempotency key
	u     *user
	rng   *rand.Rand
	n     int
	inbox inboxLog
	// lease is the lease this session asks for; renewEvery how often, in
	// real time, it renews.
	lease      time.Duration
	renewEvery time.Duration
	// events: the session's watches ask for issue events, as a live
	// board's do.
	events bool
}

// next returns a new marker, unique in the run, and an idempotency key for
// the request that carries it.
func (ss *session) next(kind string) (marker, idem string) {
	ss.n++
	return fmt.Sprintf("%s %s-%d", kind, ss.tag, ss.n), fmt.Sprintf("soak-%s-%d", ss.tag, ss.n)
}

// chance reports true with probability p.
func (ss *session) chance(p float64) bool { return ss.rng.Float64() < p }

// other returns a principal other than this session's.
func (ss *session) other() string {
	ps := slices.DeleteFunc(slices.Clone(ss.s.cfg.principals), func(p string) bool { return p == ss.key.principal })
	return ps[ss.rng.IntN(len(ps))]
}

// options are the client options of one connection of this session.
func (ss *session) options(ln *line) client.Options {
	return client.Options{Version: "v0.2.0", Session: ss.key.session, Machine: "laptop-" + ss.key.principal,
		Getenv: func(string) string { return "" }, Harness: "claude-code", OnPush: ln.push}
}

// do sends one request on c, journals it and runs the monitor's checks.
// It does not use the work's context: a request in flight when the work
// stops is let finish.
func (s *soak) do(ss *session, c *mcpserver.RepoConn, op string, args, res any) (outcome, *proto.Error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	sent, t0 := s.clock.now(), time.Now()
	err := c.Call(ctx, op, args, res)
	cl := call{who: ss.key, op: op, args: args, sent: sent, recv: s.clock.now(), took: time.Since(t0)}
	pe, isProto := errors.AsType[*proto.Error](err)
	switch {
	case err == nil:
		cl.out = acked
		if res != nil {
			cl.res = reflect.ValueOf(res).Elem().Interface()
		}
	case c.Err() != nil:
		cl.out = lost
		if ctx.Err() != nil {
			s.mon.fail("", "%s: %s hung: no answer in %s", ss.key, op, callTimeout)
		}
	case isProto:
		cl.out, cl.err = refused, pe
	default:
		cl.out = lost
		s.mon.fail("", "%s: %s failed without a protocol error on a live connection: %v", ss.key, op, err)
	}
	s.mon.record(cl)
	return cl.out, cl.err
}

// pause waits d of real time, or until ctx ends. It paces retries and
// renewals; nothing waits on it for a result.
func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Workload vocabulary: small sets, so caps are reached and edges and
// labels collide.
var (
	soakLabels = []string{"api", "cli", "db", "docs", "perf", "sec", "ui", "infra", "test", "ops"}
	soakPaths  = []string{"internal/a.go", "internal/b.go", "internal/c/d.go", "cmd/x/main.go", "docs/x.md",
		"docs/y.md", "go.mod", "internal/e.go", "internal/f/g.go", "README.md", "internal/h.go", "test/z.sh",
		"internal/i.go", "internal/j.go"}
	soakModels = []string{"claude-sonnet-4-5", "claude-haiku-4-5", "gpt-5-codex"}
)

// pathsSample returns up to n distinct paths, most recent first, as git
// would report them.
func (ss *session) pathsSample(n int) []string {
	p := slices.Clone(soakPaths)
	ss.rng.Shuffle(len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
	return p[:min(n, len(p))]
}

// acceptance returns acceptance text of n list items.
func acceptance(marker string, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "- item %d of %s\n", i+1, marker)
	}
	return b.String()
}

// libSession is an agent using the client library directly.
type libSession struct {
	*session
	conn  *mcpserver.RepoConn
	ln    *line
	conns int
	// held maps an issue to the epoch this session took it at and its
	// acceptance item count.
	held map[string]heldIssue
	// revs remembers a revision seen per issue, which may be stale by
	// the time it is used.
	revs    map[string]int64
	renewed time.Time
}

// heldIssue is an issue a session believes it holds.
type heldIssue struct {
	epoch int64
	items int
}

// connect dials until it has a watched connection or ctx ends.
func (ls *libSession) connect(ctx context.Context) bool {
	for ctx.Err() == nil {
		ls.conns++
		ln := ls.inbox.newLine(ls.key, ls.conns, ls.s.mon)
		dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		c, err := mcpserver.DialRepo(dctx, ls.u.repo, ls.options(ln))
		cancel()
		if err != nil {
			ls.s.mon.dialFailed(ls.key, err)
			pause(ctx, time.Duration(20+ls.rng.IntN(60))*time.Millisecond)
			continue
		}
		var wr proto.WatchResult
		ln.watching(ls.events)
		if out, _ := ls.s.do(ls.session, c, proto.OpWatch, proto.WatchArgs{Events: ls.events}, &wr); out == acked {
			ln.watched(ls.s.clock.now())
		}
		if c.Err() != nil {
			_ = c.Close()
			continue
		}
		ls.conn, ls.ln = c, ln
		return true
	}
	return false
}

// drop closes the connection, as a crashed process or a reconnect would.
func (ls *libSession) drop() {
	if ls.conn == nil {
		return
	}
	ls.ln.broke()
	_ = ls.conn.Close()
	ls.conn, ls.ln = nil, nil
}

// call sends one request, reconnecting first if the connection is gone.
// With retry, a request whose connection failed is sent again, the same,
// until it is answered: only for requests that are safe to repeat, those
// with an idempotency key and those that change nothing twice.
func (ls *libSession) call(ctx context.Context, op string, args, res any, retry bool) (outcome, *proto.Error) {
	for attempt := 0; ; attempt++ {
		if ls.conn == nil || ls.conn.Err() != nil {
			if ls.conn != nil {
				ls.drop()
			}
			if !ls.connect(ctx) {
				return lost, nil
			}
		}
		out, perr := ls.s.do(ls.session, ls.conn, op, args, res)
		if out != lost {
			return out, perr
		}
		ls.drop()
		if !retry || attempt >= 4 || ctx.Err() != nil {
			return lost, nil
		}
		ls.s.mon.retried(op)
	}
}

// run works until ctx ends or the session vanishes. It reports whether
// the session is still alive, with its connection to be settled.
func (ls *libSession) run(ctx context.Context) bool {
	for ctx.Err() == nil {
		if time.Since(ls.renewed) >= ls.renewEvery {
			ls.renew(ctx)
		}
		if ls.ln != nil && ls.ln.resynced() {
			ls.rewatch(ctx)
		}
		ls.step(ctx)
		switch {
		case ls.chance(ls.s.cfg.vanish):
			ls.drop() // the process died: no release, no renewals
			ls.s.mon.vanished(ls.key)
			return false
		case ls.chance(ls.s.cfg.stall):
			ls.stall(ctx)
		case ls.chance(0.01) && ls.conn != nil:
			ls.s.checkWindow(ls.session, ls.ln, ls.conn) // a reconnect: owed pushes first
			ls.drop()
		}
		pause(ctx, time.Duration(ls.rng.IntN(int(ls.s.cfg.think)+1)))
	}
	return true
}

// step does one thing an agent might.
func (ls *libSession) step(ctx context.Context) {
	ops := []struct {
		w  int
		fn func(context.Context)
	}{
		{ls.s.createWeight(), ls.create},
		{6, ls.startTop},
		{5, ls.startNamed},
		{3, ls.takeOver},
		{7, ls.finish},
		{3, ls.handoff},
		{4, ls.dep},
		{5, ls.comment},
		{4, ls.label},
		{5, ls.update},
		{2, ls.closeIssue},
		{1, ls.reopen},
		{2, ls.accept},
		{3, ls.usage},
		{2, ls.readInbox},
		{4, ls.read},
	}
	total := 0
	for _, o := range ops {
		total += o.w
	}
	n := ls.rng.IntN(total)
	for _, o := range ops {
		if n < o.w {
			o.fn(ctx)
			return
		}
		n -= o.w
	}
}

func (ls *libSession) renew(ctx context.Context) {
	ls.renewed = time.Now()
	args := proto.RenewArgs{Lease: proto.Span(ls.lease)}
	if len(ls.held) > 0 && ls.chance(0.5) {
		args.Paths = map[string][]string{}
		for id := range ls.held {
			args.Paths[id] = ls.pathsSample(1 + ls.rng.IntN(len(soakPaths)))
		}
	}
	var r proto.ClaimsResult
	if out, _ := ls.call(ctx, proto.OpRenew, args, &r, true); out != acked {
		return
	}
	still := map[string]bool{}
	for _, c := range r.Claims {
		still[c.ID] = true
	}
	for id, h := range ls.held {
		if !still[id] {
			delete(ls.held, id) // lost: the inbox says so too
			ls.s.mon.forgot(ls.key, id, h.epoch)
		}
	}
}

// heldIssue returns one issue this session believes it holds.
func (ls *libSession) someHeld() (string, heldIssue, bool) {
	if len(ls.held) == 0 {
		return "", heldIssue{}, false
	}
	ids := slices.Sorted(maps.Keys(ls.held))
	id := ids[ls.rng.IntN(len(ids))]
	return id, ls.held[id], true
}

func (ls *libSession) create(ctx context.Context) {
	title, idem := ls.next("task")
	args := proto.CreateArgs{Title: title, Idem: idem}
	p := ls.rng.IntN(5)
	args.Priority = &p
	if n := ls.rng.IntN(4); n > 0 {
		for range n {
			args.Labels = append(args.Labels, soakLabels[ls.rng.IntN(len(soakLabels))])
		}
	}
	switch {
	case ls.chance(0.05):
		args.Acceptance = acceptance(title, ls.s.cfg.limits.AcceptanceItems+1) // past the cap: refused
	case ls.chance(0.6):
		args.Acceptance = acceptance(title, 1+ls.rng.IntN(3))
	}
	if ls.chance(0.3) {
		args.Paths = ls.pathsSample(1 + ls.rng.IntN(3))
		if ls.chance(0.3) {
			args.Paths = append(args.Paths, "docs/")
		}
	}
	if ls.chance(0.15) {
		if id, ok := ls.s.pool.pick(ls.rng); ok {
			args.Parent = id
		}
	}
	if ls.chance(0.1) {
		args.Assignee = ls.other()
	}
	var r proto.CreateResult
	if out, _ := ls.call(ctx, proto.OpCreate, args, &r, true); out == acked {
		ls.s.pool.add(r.ID)
		ls.revs[r.ID] = r.Rev
	}
}

func (ls *libSession) startTop(ctx context.Context) {
	ls.start(ctx, "", false)
}

func (ls *libSession) startNamed(ctx context.Context) {
	id, ok := ls.s.pool.pick(ls.rng)
	if !ok {
		return
	}
	ls.start(ctx, id, ls.chance(0.3))
}

func (ls *libSession) start(ctx context.Context, id string, take bool) {
	var r proto.StartResult
	args := proto.StartArgs{ID: id, Lease: proto.Span(ls.lease), Take: take}
	// A start by the same session only extends its own lease, so it is
	// safe to send again.
	if out, _ := ls.call(ctx, proto.OpStart, args, &r, true); out == acked && r.Claim != nil {
		ls.held[r.Issue.ID] = heldIssue{epoch: r.Claim.Epoch, items: len(r.Items)}
		ls.revs[r.Issue.ID] = r.Issue.Rev
		ls.s.holding.took(r.Issue.ID, ls.key)
	}
}

// rewatch watches again after a resync, as a client following its inbox
// or a board does.
func (ls *libSession) rewatch(ctx context.Context) {
	ls.s.stats.rewatches.Add(1)
	var wr proto.WatchResult
	ls.ln.watching(ls.events)
	if out, _ := ls.call(ctx, proto.OpWatch, proto.WatchArgs{Events: ls.events}, &wr, false); out == acked && ls.ln != nil {
		ls.ln.watched(ls.s.clock.now())
	}
}

// takeOver starts an issue another session holds, as far as the run
// knows: one of its own principal's, which it may take when it asks to,
// or another principal's, which it must be refused.
func (ls *libSession) takeOver(ctx context.Context) {
	if id, ok := ls.s.holding.pick(ls.rng, ls.key, ls.chance(0.7)); ok {
		ls.start(ctx, id, ls.chance(0.8))
	}
}

// stall stops the session past its lease without a word, as a process
// stopped in a debugger or a laptop gone to sleep: its claims lapse and
// others take them. It then carries on with what it believes it still
// holds, which fencing must refuse once another holder has the issue.
func (ls *libSession) stall(ctx context.Context) {
	ls.s.stats.stalls.Add(1)
	pause(ctx, ls.s.clock.real(ls.lease+2*time.Minute))
	if ls.chance(0.5) {
		ls.finish(ctx)
	} else {
		ls.handoff(ctx)
	}
}

func (ls *libSession) finish(ctx context.Context) {
	id, h, ok := ls.someHeld()
	if !ok {
		return
	}
	reason, idem := ls.next("done")
	args := proto.FinishArgs{ID: id, Epoch: h.epoch, Reason: reason, Idem: idem, Paths: ls.pathsSample(ls.rng.IntN(4))}
	if h.epoch > 1 && ls.chance(0.05) {
		args.Epoch = h.epoch - 1 // a stale epoch: refused
	}
	for n := 1; n <= h.items; n++ {
		if n == h.items && ls.chance(0.1) {
			break // one item left open: refused
		}
		args.Ticked = append(args.Ticked, n)
	}
	for k := range ls.rng.IntN(3) {
		pr := ls.rng.IntN(5)
		args.Discovered = append(args.Discovered, proto.Discovered{Title: fmt.Sprintf("found %s-%d-%d", ls.tag, ls.n, k), Priority: &pr})
	}
	if ls.chance(0.4) {
		args.Handoff = fmt.Sprintf("handoff %s-%d by %s, @%s please review", ls.tag, ls.n, ls.key.principal, ls.other())
		args.State, args.Next = "done", "review it"
		if ls.chance(0.5) {
			args.To = ls.other()
		}
	}
	var r proto.FinishResult
	out, perr := ls.call(ctx, proto.OpFinish, args, &r, true)
	switch {
	case out == acked:
		delete(ls.held, id)
		ls.s.holding.left(id, ls.key)
		for _, c := range r.Created {
			ls.s.pool.add(c)
		}
	case out == refused && perr.Code != proto.CodeAcceptance && perr.Code != proto.CodeBusy:
		delete(ls.held, id) // lost it, or it is closed
	}
}

func (ls *libSession) handoff(ctx context.Context) {
	id, h, ok := ls.someHeld()
	if !ok {
		return
	}
	note, idem := ls.next("handoff")
	args := proto.HandoffArgs{ID: id, Note: note + " @" + ls.other(), Idem: idem, Release: ls.chance(0.6),
		Paths: ls.pathsSample(ls.rng.IntN(3))}
	if args.Release {
		args.Epoch = h.epoch
	}
	if ls.chance(0.5) {
		args.State, args.To = "partial", ls.other()
	}
	var r proto.WriteResult
	if out, _ := ls.call(ctx, proto.OpHandoff, args, &r, true); out == acked && args.Release {
		delete(ls.held, id)
		ls.s.holding.left(id, ls.key)
	}
}

func (ls *libSession) dep(ctx context.Context) {
	from, ok1 := ls.s.pool.pick(ls.rng)
	to, ok2 := ls.s.pool.pick(ls.rng)
	if !ok1 || !ok2 {
		return
	}
	op, typ := proto.OpDepAdd, "blocks"
	if ls.chance(0.3) {
		op = proto.OpDepRm
	}
	if ls.chance(0.2) {
		typ = "related"
	}
	// Not resent: a second dep.rm could remove an edge someone added
	// since, as sfx mcp knows (its dep tool does not retry either).
	_, _ = ls.call(ctx, op, proto.DepArgs{From: from, To: to, Type: typ}, nil, false)
}

func (ls *libSession) comment(ctx context.Context) {
	id, ok := ls.s.pool.pick(ls.rng)
	if !ok {
		return
	}
	body, idem := ls.next("note")
	if ls.chance(0.3) {
		body += " @" + ls.other()
	}
	var r proto.CommentResult
	_, _ = ls.call(ctx, proto.OpComment, proto.CommentArgs{ID: id, Body: body, Idem: idem}, &r, true)
}

func (ls *libSession) label(ctx context.Context) {
	id, ok := ls.s.pool.pick(ls.rng)
	if !ok {
		return
	}
	op := proto.OpLabelAdd
	if ls.chance(0.35) {
		op = proto.OpLabelRm
	}
	_, _ = ls.call(ctx, op, proto.LabelArgs{ID: id, Label: soakLabels[ls.rng.IntN(len(soakLabels))]}, nil, false)
}

// update edits an issue at a revision it read, now or earlier: an old
// one is usually stale, and refused.
func (ls *libSession) update(ctx context.Context) {
	id, ok := ls.s.pool.pick(ls.rng)
	if h, held := ls.someHeldID(); held && ls.chance(0.5) {
		id, ok = h, true
	}
	if !ok {
		return
	}
	rev, known := ls.revs[id]
	if !known || ls.chance(0.6) {
		var r proto.ShowResult
		if out, _ := ls.call(ctx, proto.OpShow, proto.ShowArgs{ID: id}, &r, true); out != acked {
			return
		}
		rev = r.Issue.Rev
	}
	title, _ := ls.next("retitled")
	args := proto.UpdateArgs{ID: id, Rev: rev, Title: &title}
	if ls.chance(0.3) {
		p := ls.rng.IntN(5)
		args.Priority = &p
	}
	switch {
	case ls.chance(0.08):
		st := []string{"open", "blocked", "deferred"}[ls.rng.IntN(3)]
		args.Status = &st
	case ls.chance(0.08):
		a := ls.other()
		args.Assignee = &a
	case ls.chance(0.05):
		if p, ok := ls.s.pool.pick(ls.rng); ok {
			args.Parent = &p
		}
	case ls.chance(0.1):
		ps := ls.pathsSample(ls.rng.IntN(4))
		args.Paths = &ps
	}
	var r proto.WriteResult
	if out, _ := ls.call(ctx, proto.OpUpdate, args, &r, false); out == acked {
		ls.revs[id] = r.Rev
	}
}

func (ls *libSession) someHeldID() (string, bool) {
	id, _, ok := ls.someHeld()
	return id, ok
}

// closeIssue ticks an issue's items and closes it, held or not: an admin
// may close what another principal holds; anyone else is refused.
func (ls *libSession) closeIssue(ctx context.Context) {
	id, ok := ls.s.pool.pick(ls.rng)
	if !ok {
		return
	}
	var sr proto.ShowResult
	if out, _ := ls.call(ctx, proto.OpShow, proto.ShowArgs{ID: id}, &sr, true); out != acked || sr.Issue.Status == "closed" {
		return
	}
	if len(sr.Items) > 0 {
		args := proto.AcceptArgs{ID: id}
		for _, it := range sr.Items {
			args.Tick = append(args.Tick, it.N)
		}
		var ar proto.AcceptResult
		_, _ = ls.call(ctx, proto.OpAccept, args, &ar, true)
	}
	reason, _ := ls.next("closed")
	var r proto.WriteResult
	if out, _ := ls.call(ctx, proto.OpClose, proto.CloseArgs{ID: id, Reason: reason}, &r, false); out == acked {
		delete(ls.held, id)
	}
}

func (ls *libSession) reopen(ctx context.Context) {
	id, ok := ls.s.pool.pick(ls.rng)
	if !ok {
		return
	}
	var r proto.WriteResult
	_, _ = ls.call(ctx, proto.OpReopen, proto.ReopenArgs{ID: id}, &r, false)
}

func (ls *libSession) accept(ctx context.Context) {
	id, h, ok := ls.someHeld()
	if !ok || h.items == 0 {
		return
	}
	n := 1 + ls.rng.IntN(h.items)
	args := proto.AcceptArgs{ID: id}
	switch {
	case ls.chance(0.2):
		args.Untick = []int{n}
	case ls.chance(0.2):
		args.Waive = map[int]string{n: "not needed here"}
	default:
		args.Tick = []int{n}
	}
	var r proto.AcceptResult
	_, _ = ls.call(ctx, proto.OpAccept, args, &r, true)
}

// usage reports token records for this session; a batch past the cap is
// refused whole, and a resent batch counts its records as duplicates.
func (ls *libSession) usage(ctx context.Context) {
	n := 1 + ls.rng.IntN(4)
	if ls.chance(0.04) {
		n = ls.s.cfg.limits.UsageRecords + 1
	}
	args := proto.UsageArgs{}
	now := ls.s.clock.now()
	for range n {
		id, _ := ls.next("u")
		in, outTok, read := int64(ls.rng.IntN(5000)), int64(ls.rng.IntN(2000)), int64(ls.rng.IntN(20000))
		rec := proto.UsageRecord{Harness: "claude-code", RequestID: strings.ReplaceAll(id, " ", "-"),
			Model: soakModels[ls.rng.IntN(len(soakModels))], Granularity: "request",
			At: now.Add(-time.Duration(ls.rng.IntN(int(ls.lease))))}
		rec.Input, rec.Output = &in, &outTok
		if ls.chance(0.7) {
			rec.CacheRead = &read
		}
		args.Records = append(args.Records, rec)
	}
	var r proto.UsageResult
	if out, _ := ls.call(ctx, proto.OpUsage, args, &r, true); out == acked && r.Added+r.Duplicates != len(args.Records) {
		ls.s.mon.fail("", "%s: usage of %d records answered %d added and %d duplicates", ls.key, len(args.Records), r.Added, r.Duplicates)
	}
}

func (ls *libSession) readInbox(ctx context.Context) {
	var r proto.InboxResult
	if out, _ := ls.call(ctx, proto.OpInbox, proto.InboxArgs{Limit: 20}, &r, true); out != acked || len(r.Items) == 0 {
		return
	}
	ids := []int64{}
	for _, it := range r.Items {
		if ls.chance(0.7) {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) > 0 {
		var ar proto.AckResult
		_, _ = ls.call(ctx, proto.OpAck, proto.AckArgs{IDs: ids}, &ar, true)
	}
}

// read runs one of the reads people and agents make all the time.
func (ls *libSession) read(ctx context.Context) {
	id, ok := ls.s.pool.pick(ls.rng)
	switch r := ls.rng.IntN(5); {
	case r == 0:
		var res proto.ListResult
		_, _ = ls.call(ctx, proto.OpReady, proto.LimitArgs{Limit: 10}, &res, true)
	case r == 1:
		var res proto.BlockedResult
		_, _ = ls.call(ctx, proto.OpBlocked, proto.LimitArgs{Limit: 10}, &res, true)
	case r == 2 && ok:
		var res proto.ShowResult
		if out, _ := ls.call(ctx, proto.OpShow, proto.ShowArgs{ID: id}, &res, true); out == acked {
			ls.revs[id] = res.Issue.Rev
		}
	case r == 3 && ok:
		var res proto.HistoryResult
		_, _ = ls.call(ctx, proto.OpHistory, proto.PageArgs{ID: id, Limit: 20}, &res, true)
	default:
		var res proto.WhoResult
		_, _ = ls.call(ctx, proto.OpWho, proto.WhoArgs{}, &res, true)
	}
}

// settle gives the session a watched connection for the end of the run,
// when the reaper tells it of the claims it held.
func (ls *libSession) settle(ctx context.Context) {
	if ls.conn == nil || ls.conn.Err() != nil {
		ls.drop()
		ls.connect(ctx)
	}
}

// liveLine returns the session's connection's line while it is up.
func (ls *libSession) liveLine() (*line, *mcpserver.RepoConn) {
	if ls.conn == nil || ls.conn.Err() != nil {
		return nil, nil
	}
	return ls.ln, ls.conn
}

func (ls *libSession) close() { ls.drop() }

// tracedConn is an mcpSession's connection: every request `sfx mcp`
// makes on it goes through soak.do, and a watch opens a window.
type tracedConn struct {
	*mcpserver.RepoConn
	ss *session
	ln *line
}

func (c *tracedConn) Call(_ context.Context, op string, args, res any) error {
	a, watch := args.(proto.WatchArgs)
	if watch {
		c.ln.watching(a.Events)
	}
	out, perr := c.ss.s.do(c.ss, c.RepoConn, op, args, res)
	switch out {
	case acked:
		if watch {
			c.ln.watched(c.ss.s.clock.now())
		}
		return nil
	case refused:
		return perr
	}
	c.ln.broke()
	if err := c.Err(); err != nil {
		return err
	}
	return errors.New("soak: request failed")
}

// mcpSession is an agent harness running `sfx mcp`.
type mcpSession struct {
	*session
	srv *mcpserver.Server
	cs  *mcp.ClientSession
	// held maps an issue this agent started to its item count.
	held  map[string]int
	mu    sync.Mutex // guards cur and conns: the server dials on its own
	cur   *tracedConn
	conns int
	// stopRenew ends the renewal loop; renewing reports it has.
	stopRenew context.CancelFunc
	renewing  chan struct{}
}

// open starts the MCP server and client for this session and its renewal
// loop, as a harness starting `sfx mcp` would.
func (ms *mcpSession) open() error {
	ms.srv = mcpserver.New(mcpserver.Options{Version: "v0.2.0", RenewEvery: -1, Dir: ms.u.repo,
		Dial: func(ctx context.Context) (mcpserver.Conn, error) {
			ms.mu.Lock()
			ms.conns++
			ln := ms.inbox.newLine(ms.key, ms.conns, ms.s.mon)
			ms.mu.Unlock()
			opts := ms.options(ln)
			opts.OnPush = func(p proto.Push) {
				ln.push(p)
				ms.srv.Push(p)
			}
			c, err := mcpserver.DialRepo(ctx, ms.u.repo, opts)
			if err != nil {
				ms.s.mon.dialFailed(ms.key, err)
				return nil, err
			}
			tc := &tracedConn{RepoConn: c, ss: ms.session, ln: ln}
			ms.mu.Lock()
			ms.cur = tc
			ms.mu.Unlock()
			return tc, nil
		}})
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := ms.srv.MCP().Connect(ctx, st, nil); err != nil {
		return err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "soak"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return err
	}
	ms.cs = cs
	rctx, cancel := context.WithCancel(context.Background())
	ms.stopRenew, ms.renewing = cancel, make(chan struct{})
	go func() {
		defer close(ms.renewing)
		for rctx.Err() == nil {
			pause(rctx, ms.renewEvery)
			ms.srv.Renew(rctx)
		}
	}()
	return nil
}

// tool calls one tool and returns its first text block and whether it
// was an error. A failure of the MCP transport itself is a soak failure.
func (ms *mcpSession) tool(name string, args map[string]any) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*callTimeout)
	defer cancel()
	res, err := ms.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		ms.s.mon.fail("", "%s: tool %s: %v", ms.key, name, err)
		return "", true
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	return text, res.IsError
}

// run works until ctx ends or the agent vanishes; it reports whether the
// agent is still alive.
func (ms *mcpSession) run(ctx context.Context) bool {
	for ctx.Err() == nil {
		ms.step()
		if ms.chance(ms.s.cfg.vanish) {
			ms.stop() // the harness was killed: no release
			ms.s.mon.vanished(ms.key)
			return false
		}
		pause(ctx, time.Duration(ms.rng.IntN(int(ms.s.cfg.think)+1)))
	}
	return true
}

func (ms *mcpSession) step() {
	switch n := ms.rng.IntN(100); {
	case n < 3:
		ms.tool("prime", nil)
	case n < 18:
		ms.start("", false)
	case n < 25:
		if id, ok := ms.s.pool.pick(ms.rng); ok {
			ms.start(id, ms.chance(0.3))
		}
	case n < 28:
		if id, ok := ms.s.holding.pick(ms.rng, ms.key, ms.chance(0.7)); ok {
			ms.start(id, ms.chance(0.8))
		}
	case n < 42:
		ms.finish()
	case n < 48:
		ms.handoff()
	case n < 58:
		ms.create()
	case n < 66:
		if id, ok := ms.s.pool.pick(ms.rng); ok {
			body, _ := ms.next("note")
			if ms.chance(0.3) {
				body += " @" + ms.other()
			}
			ms.tool("comment", map[string]any{"id": id, "body": body})
		}
	case n < 71:
		if id, ok := ms.s.pool.pick(ms.rng); ok {
			action := "add"
			if ms.chance(0.35) {
				action = "rm"
			}
			ms.tool("label", map[string]any{"action": action, "id": id, "labels": []any{soakLabels[ms.rng.IntN(len(soakLabels))]}})
		}
	case n < 76:
		from, ok1 := ms.s.pool.pick(ms.rng)
		to, ok2 := ms.s.pool.pick(ms.rng)
		if ok1 && ok2 {
			action := "add"
			if ms.chance(0.3) {
				action = "rm"
			}
			ms.tool("dep", map[string]any{"action": action, "id": from, "depends_on": to})
		}
	case n < 84:
		if id, ok := ms.s.pool.pick(ms.rng); ok {
			title, _ := ms.next("retitled")
			ms.tool("update", map[string]any{"id": id, "title": title})
		}
	case n < 90:
		ms.readInbox()
	case n < 95:
		if id, ok := ms.s.pool.pick(ms.rng); ok {
			ms.tool("show", map[string]any{"id": id})
		}
	default:
		ms.tool("ready", nil)
	}
}

func (ms *mcpSession) create() {
	title, _ := ms.next("task")
	args := map[string]any{"title": title, "priority": ms.rng.IntN(5)}
	if ms.chance(0.6) {
		args["acceptance"] = acceptance(title, 1+ms.rng.IntN(3))
	}
	if ms.chance(0.3) {
		args["labels"] = []any{soakLabels[ms.rng.IntN(len(soakLabels))]}
	}
	text, isErr := ms.tool("create", args)
	if isErr {
		return
	}
	var c mcpserver.Created
	if json.Unmarshal([]byte(text), &c) == nil && c.ID != "" {
		ms.s.pool.add(c.ID)
	}
}

func (ms *mcpSession) start(id string, take bool) {
	args := map[string]any{}
	if id != "" {
		args["id"] = id
	}
	if take {
		args["take"] = true
	}
	text, isErr := ms.tool("start", args)
	if isErr {
		return
	}
	var st mcpserver.Started
	if json.Unmarshal([]byte(text), &st) == nil && st.ID != "" {
		ms.held[st.ID] = len(st.Items)
		ms.s.holding.took(st.ID, ms.key)
	}
}

func (ms *mcpSession) someHeld() (string, int, bool) {
	if len(ms.held) == 0 {
		return "", 0, false
	}
	ids := slices.Sorted(maps.Keys(ms.held))
	id := ids[ms.rng.IntN(len(ids))]
	return id, ms.held[id], true
}

func (ms *mcpSession) finish() {
	id, items, ok := ms.someHeld()
	if !ok {
		return
	}
	reason, _ := ms.next("done")
	args := map[string]any{"id": id, "reason": reason}
	var ticked []any
	for n := 1; n <= items; n++ {
		ticked = append(ticked, n)
	}
	if len(ticked) > 0 {
		args["ticked"] = ticked
	}
	var found []any
	for k := range ms.rng.IntN(3) {
		found = append(found, map[string]any{"title": fmt.Sprintf("found %s-%d-%d", ms.tag, ms.n, k)})
	}
	if len(found) > 0 {
		args["discovered"] = found
	}
	if ms.chance(0.4) {
		args["handoff"] = fmt.Sprintf("handoff %s-%d by %s, @%s please review", ms.tag, ms.n, ms.key.principal, ms.other())
		args["to"] = ms.other()
	}
	text, isErr := ms.tool("finish", args)
	if !isErr || !strings.HasPrefix(text, "acceptance") {
		delete(ms.held, id)
	}
	if isErr {
		return
	}
	var r proto.FinishResult
	if json.Unmarshal([]byte(text), &r) == nil {
		for _, c := range r.Created {
			ms.s.pool.add(c)
		}
	}
}

func (ms *mcpSession) handoff() {
	id, _, ok := ms.someHeld()
	if !ok {
		return
	}
	note, _ := ms.next("handoff")
	release := ms.chance(0.6)
	args := map[string]any{"id": id, "note": note + " @" + ms.other(), "release": release, "state": "partial"}
	if _, isErr := ms.tool("handoff", args); !isErr && release {
		delete(ms.held, id)
	}
}

func (ms *mcpSession) readInbox() {
	text, isErr := ms.tool("inbox", nil)
	if isErr {
		return
	}
	var box mcpserver.Inbox
	if json.Unmarshal([]byte(text), &box) != nil || len(box.Items) == 0 {
		return
	}
	var ids []any
	for _, it := range box.Items {
		ids = append(ids, it.ID)
	}
	ms.tool("inbox", map[string]any{"ack": ids})
}

// stop ends the renewal loop and closes the agent's connection, as a
// harness exiting does.
func (ms *mcpSession) stop() {
	if ms.stopRenew != nil {
		ms.stopRenew()
		<-ms.renewing
		ms.stopRenew = nil
	}
	if ms.cs != nil {
		_ = ms.cs.Close()
		ms.cs = nil
	}
	_ = ms.srv.Close()
	ms.mu.Lock()
	if ms.cur != nil {
		ms.cur.ln.broke()
		ms.cur = nil
	}
	ms.mu.Unlock()
}

// settle stops renewing, so the agent's claims lapse, and makes sure it
// has a watched connection: a read redials if the last one dropped.
func (ms *mcpSession) settle(context.Context) {
	if ms.stopRenew != nil {
		ms.stopRenew()
		<-ms.renewing
		ms.stopRenew = nil
	}
	ms.tool("ready", nil)
}

func (ms *mcpSession) liveLine() (*line, *mcpserver.RepoConn) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.cur == nil || ms.cur.Err() != nil {
		return nil, nil
	}
	return ms.cur.ln, ms.cur.RepoConn
}

func (ms *mcpSession) close() { ms.stop() }

// worker is either kind of session, as the soak's end needs it.
type worker interface {
	base() *session
	settle(context.Context)
	liveLine() (*line, *mcpserver.RepoConn)
	close()
}

func (ls *libSession) base() *session { return ls.session }
func (ms *mcpSession) base() *session { return ms.session }

// pool is the issue ids the workload knows, for picking at random.
type pool struct {
	mu  sync.Mutex
	ids []string
}

func (p *pool) add(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, id)
}

// pick returns a random id, favoring recent ones.
func (p *pool) pick(rng *rand.Rand) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) == 0 {
		return "", false
	}
	recent := p.ids[max(0, len(p.ids)-200):]
	if rng.IntN(4) == 0 {
		return p.ids[rng.IntN(len(p.ids))], true
	}
	return recent[rng.IntN(len(recent))], true
}

// holding is who holds what, as the sessions last heard: a start they
// were answered, and a finish or release. Leases that lapse leave stale
// entries, which only make a take-over attempt find the issue free.
type holding struct {
	mu sync.Mutex
	by map[string]sessKey
}

func (h *holding) took(id string, who sessKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.by == nil {
		h.by = map[string]sessKey{}
	}
	h.by[id] = who
}

func (h *holding) left(id string, who sessKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.by[id] == who {
		delete(h.by, id)
	}
}

// pick returns an issue held by another session than me: of my principal
// when same is set, of another principal otherwise.
func (h *holding) pick(rng *rand.Rand, me sessKey, same bool) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for id, who := range h.by {
		if who != me && (who.principal == me.principal) == same {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", false
	}
	slices.Sort(ids)
	return ids[rng.IntN(len(ids))], true
}
