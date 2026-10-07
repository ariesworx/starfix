package server

import (
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

func TestDispatchInbox(t *testing.T) {
	s := newServer(t)
	mustCall[proto.ClaimsResult](t, s, bob, proto.OpRenew, proto.RenewArgs{}) // bob is known: he has connected
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "@bob have a look"})
	mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "yours", Assignee: "bob"})

	in := mustCall[proto.InboxResult](t, s, bob, proto.OpInbox, proto.InboxArgs{})
	if in.Unread != 2 || len(in.Items) != 2 || in.Items[0].Kind != "assigned" || in.Items[1].Kind != "mention" ||
		in.Items[1].Issue != a.ID || in.Items[1].From != "alice" || in.Items[1].Body != "@bob have a look" {
		t.Fatalf("inbox = %+v, want an assignment then a mention, newest first", in)
	}
	if got := mustCall[proto.AckResult](t, s, bob, proto.OpAck, proto.AckArgs{IDs: []int64{in.Items[1].ID}}); got.Acked != 1 {
		t.Fatalf("ack = %+v, want 1", got)
	}
	if got := mustCall[proto.InboxResult](t, s, bob, proto.OpInbox, proto.InboxArgs{All: true}); got.Unread != 1 ||
		len(got.Items) != 2 || got.Items[1].ReadAt == nil {
		t.Fatalf("inbox all = %+v, want both, one read", got)
	}
	if got := mustCall[proto.AckResult](t, s, bob, proto.OpAck, proto.AckArgs{All: true}); got.Acked != 1 {
		t.Fatalf("ack all = %+v, want 1", got)
	}
	if got := mustCall[proto.InboxResult](t, s, alice, proto.OpInbox, proto.InboxArgs{}); got.Unread != 0 || len(got.Items) != 0 {
		t.Fatalf("alice's inbox = %+v, want empty: she wrote both", got)
	}

	for _, tc := range []struct {
		name string
		op   string
		args any
		fix  string
	}{
		{"ack nothing", proto.OpAck, proto.AckArgs{}, "sfx inbox -h"},
		{"inbox limit", proto.OpInbox, proto.InboxArgs{Limit: 101}, "sfx inbox -h"},
		{"watch without a connection", proto.OpWatch, proto.WatchArgs{}, "watch"},
	} {
		_, perr := call[proto.Empty](t, s, bob, tc.op, tc.args)
		if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, tc.fix) {
			t.Errorf("%s = %+v, want invalid with a fix naming %q", tc.name, perr, tc.fix)
		}
	}
}

// Handoff fields go in through finish and handoff and come back from
// start and show.
func TestDispatchStructuredHandoff(t *testing.T) {
	s := newServer(t)
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: a.ID})
	f := proto.HandoffFields{State: "partial", Next: "wire the CLI", Branch: "feature/sf-1-work", Worktree: "/src/work", To: "bob"}
	mustCall[proto.WriteResult](t, s, alice, proto.OpHandoff, proto.HandoffArgs{ID: a.ID, Note: "half done", Release: true, HandoffFields: f})

	show := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: a.ID})
	if h := show.Handoff; h == nil || h.Body != "half done" || h.Author != "alice" || h.HandoffFields != f {
		t.Fatalf("show handoff = %+v, want the note with %+v", show.Handoff, f)
	}
	st := mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{ID: a.ID})
	if h := st.Handoff; h == nil || h.Kind != "handoff" || h.HandoffFields != f {
		t.Fatalf("start handoff = %+v, want %+v", st.Handoff, f)
	}
	in := mustCall[proto.InboxResult](t, s, bob, proto.OpInbox, proto.InboxArgs{})
	if len(in.Items) != 1 || in.Items[0].Kind != "handoff" || in.Items[0].Body != "wire the CLI" {
		t.Fatalf("bob's inbox = %+v, want the handoff, quoting next", in)
	}
	_, perr := call[proto.Empty](t, s, bob, proto.OpFinish, proto.FinishArgs{ID: a.ID, HandoffFields: proto.HandoffFields{State: "done"}})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "need a note") {
		t.Fatalf("finish with fields and no note = %+v", perr)
	}
	mustCall[proto.FinishResult](t, s, bob, proto.OpFinish, proto.FinishArgs{ID: a.ID, Handoff: "shipped",
		HandoffFields: proto.HandoffFields{State: "done", To: "alice"}})
	if in := mustCall[proto.InboxResult](t, s, alice, proto.OpInbox, proto.InboxArgs{}); len(in.Items) != 1 || in.Items[0].Kind != "handoff" {
		t.Fatalf("alice's inbox after finish = %+v", in)
	}
}

// conn is a client of s over net.Pipe, past the handshake.
type conn struct {
	t      *testing.T
	enc    *proto.Encoder
	dec    *proto.Decoder
	nc     net.Conn
	nextID uint64
}

func dialPipe(t *testing.T, s *Server, a store.Actor) *conn {
	t.Helper()
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handle(t.Context(), srv)
	}()
	t.Cleanup(func() { _ = cli.Close(); <-done })
	c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	go func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: a.Principal})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: 2, Project: project, Session: a.Session, Machine: a.Machine})
	}()
	if f := c.read(); f.T != proto.FrameWelcome || f.Err != nil {
		t.Fatalf("welcome: %+v", f)
	}
	return c
}

func (c *conn) read() *proto.Frame {
	c.t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := c.dec.Decode()
	if err != nil {
		c.t.Fatalf("read frame: %v", err)
	}
	return f
}

// send writes a request without waiting, since net.Pipe blocks a write
// until the other side reads.
func (c *conn) send(op string, args any) uint64 {
	c.t.Helper()
	c.nextID++
	f, err := proto.Request(c.nextID, op, args)
	if err != nil {
		c.t.Fatal(err)
	}
	go func() { _ = c.enc.Encode(f) }()
	return c.nextID
}

// frames reads until the response to id, returning the pushes before it.
func (c *conn) until(id uint64) []proto.Push {
	c.t.Helper()
	var out []proto.Push
	for {
		f := c.read()
		switch f.T {
		case proto.FrameRes:
			if f.ID != id || f.Err != nil {
				c.t.Fatalf("response %+v, want ok for request %d", f, id)
			}
			return out
		case proto.FrameEvent:
			p, err := proto.DecodePush(f)
			if err != nil {
				c.t.Fatal(err)
			}
			out = append(out, p)
		default:
			c.t.Fatalf("unexpected frame %+v", f)
		}
	}
}

func kinds(ps []proto.Push) []string {
	var out []string
	for _, p := range ps {
		if p.Item != nil {
			out = append(out, p.Op+":"+p.Item.Kind)
		} else {
			out = append(out, p.Op)
		}
	}
	return out
}

// A watching connection is pushed each new item for its principal and
// session, and no one else's; an item committed before a response is
// pushed before it. A connection that never watches is pushed nothing.
func TestPushFanOut(t *testing.T) {
	s := newServer(t)
	b := dialPipe(t, s, bob)
	other := dialPipe(t, s, store.Actor{Principal: "carol", Session: "s-c", Machine: "m"})
	idle := dialPipe(t, s, store.Actor{Principal: "bob", Session: "s-idle", Machine: "m"})
	if got := b.until(b.send(proto.OpWatch, proto.WatchArgs{})); len(got) != 0 {
		t.Fatalf("pushes before anything happened: %v", kinds(got))
	}
	other.until(other.send(proto.OpWatch, proto.WatchArgs{}))

	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "@bob look"})
	mustStart := func(who store.Actor) {
		mustCall[proto.StartResult](t, s, who, proto.OpStart, proto.StartArgs{ID: a.ID})
	}
	mustStart(bob)
	mustStart(store.Actor{Principal: "bob", Session: "s-b2", Machine: "m"}) // takes over bob's claim

	got := b.until(b.send(proto.OpWho, proto.WhoArgs{}))
	if want := []string{"inbox:mention", "inbox:claim.lost"}; !slices.Equal(kinds(got), want) {
		t.Fatalf("bob's pushes before his next response = %v, want %v", kinds(got), want)
	}
	if it := got[1].Item; it.Session != bob.Session || it.Issue != a.ID || it.ID == 0 {
		t.Errorf("claim.lost push = %+v, want bob's session and the issue", it)
	}
	if got := other.until(other.send(proto.OpWho, proto.WhoArgs{})); len(got) != 0 {
		t.Errorf("carol was pushed bob's items: %v", kinds(got))
	}
	if got := idle.until(idle.send(proto.OpWho, proto.WhoArgs{})); len(got) != 0 {
		t.Errorf("a connection that never watched was pushed %v", kinds(got))
	}
	// Watching again while watched changes nothing.
	if got := b.until(b.send(proto.OpWatch, proto.WatchArgs{})); len(got) != 0 {
		t.Errorf("rewatch pushed %v", kinds(got))
	}
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "@bob again"})
	if got := b.until(b.send(proto.OpWho, proto.WhoArgs{})); len(got) != 1 {
		t.Errorf("after a rewatch, one mention pushed %v times", len(got))
	}
}

// A connection that stops reading never holds up the writer: its queue
// overflows, it is sent one resync and nothing more until it watches
// again.
func TestPushOverflowResyncs(t *testing.T) {
	s := newServer(t)
	b := dialPipe(t, s, bob)
	b.until(b.send(proto.OpWatch, proto.WatchArgs{}))
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})

	// bob reads nothing while more than two queues' worth is written, so
	// the queue overflows however many the pusher took before blocking.
	for range 2*store.WatchQueue + 1 {
		mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "@bob ping"})
	}
	var items int
	for {
		f := b.read()
		p, err := proto.DecodePush(f)
		if err != nil {
			t.Fatalf("frame %+v: %v", f, err)
		}
		if p.Op == proto.EvResync {
			break
		}
		items++
	}
	if items > store.WatchQueue {
		t.Errorf("%d items pushed before the resync, more than the queue holds (%d)", items, store.WatchQueue)
	}
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "@bob lost"})
	if got := b.until(b.send(proto.OpWho, proto.WhoArgs{})); len(got) != 0 {
		t.Fatalf("pushes after the resync: %v", kinds(got))
	}
	w := b.until(b.send(proto.OpWatch, proto.WatchArgs{}))
	if len(w) != 0 {
		t.Fatalf("rewatch pushed %v", kinds(w))
	}
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "@bob back"})
	if got := b.until(b.send(proto.OpWho, proto.WhoArgs{})); !slices.Equal(kinds(got), []string{"inbox:mention"}) {
		t.Fatalf("after rewatching, pushes = %v, want one mention", kinds(got))
	}
}
