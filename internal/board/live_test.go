package board

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// fakeServer answers the board's reads and records them, as "op" or
// "show ID". Each dial is a new fakeConn.
type fakeServer struct {
	mu     sync.Mutex // guards the fields below
	calls  []string
	conns  []*fakeConn
	refuse map[string]*proto.Error // by op
	push   func(proto.Push)
	// gate, when set, holds every call until it is closed.
	gate chan struct{}
}

func (f *fakeServer) dial(_ context.Context, onPush func(proto.Push)) (Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeConn{srv: f, done: make(chan struct{})}
	f.conns, f.push = append(f.conns, c), onPush
	return c, nil
}

// took returns the calls made since the last took.
func (f *fakeServer) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func (f *fakeServer) send(p proto.Push) {
	f.mu.Lock()
	push := f.push
	f.mu.Unlock()
	push(p)
}

func (f *fakeServer) last() *fakeConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[len(f.conns)-1]
}

var errLost = errors.New("the server closed the connection")

type fakeConn struct {
	srv  *fakeServer
	done chan struct{}
	once sync.Once
}

func (c *fakeConn) Done() <-chan struct{} { return c.done }

func (c *fakeConn) Err() error {
	select {
	case <-c.done:
		return errLost
	default:
		return nil
	}
}

func (c *fakeConn) Close() error { c.drop(); return nil }

func (c *fakeConn) drop() { c.once.Do(func() { close(c.done) }) }

func (c *fakeConn) Call(_ context.Context, op string, args, result any) error {
	if err := c.Err(); err != nil {
		return err
	}
	c.srv.mu.Lock()
	gate := c.srv.gate
	c.srv.mu.Unlock()
	if gate != nil {
		<-gate
	}
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	name := op
	if a, ok := args.(proto.ShowArgs); ok {
		name += " " + a.ID
	}
	c.srv.calls = append(c.srv.calls, name)
	if e := c.srv.refuse[op]; e != nil {
		return e
	}
	switch r := result.(type) {
	case *proto.ListResult:
		if op == proto.OpReady {
			*r = proto.ListResult{Issues: []proto.Summary{{ID: "sf-r1", Title: "ready"}}}
		} else {
			*r = proto.ListResult{Issues: []proto.Summary{{ID: "sf-h1", Title: "held", Priority: 1}}}
		}
	case *proto.BlockedResult:
		*r = proto.BlockedResult{}
	case *proto.ClaimsResult:
		*r = proto.ClaimsResult{Claims: []proto.Claim{{ID: "sf-h1", By: "alice", Session: "s1"}}, Now: time.Now()}
	case *proto.ShowResult:
		*r = proto.ShowResult{Issue: proto.Issue{ID: args.(proto.ShowArgs).ID}}
	}
	return nil
}

// board runs Live against a fake server until the test ends, collecting
// its updates.
type liveRun struct {
	srv     *fakeServer
	actions chan Action
	updates chan Update
}

func startLive(t *testing.T, srv *fakeServer) *liveRun {
	t.Helper()
	l := NewLive(srv.dial)
	conn, err := l.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r := &liveRun{srv: srv, actions: make(chan Action), updates: make(chan Update, 1000)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx, conn, r.actions, r.updates)
	}()
	t.Cleanup(func() { cancel(); <-done })
	synctest.Wait()
	return r
}

// got drains the updates sent so far, named by kind.
func (r *liveRun) got() []string {
	var out []string
	for {
		select {
		case u := <-r.updates:
			switch u := u.(type) {
			case *Snapshot:
				out = append(out, "snapshot")
			case Pushed:
				out = append(out, "event "+u.Event.Op)
			case Status:
				out = append(out, "status "+u.State)
			case *Detail:
				out = append(out, "detail "+u.ID)
			}
		default:
			return out
		}
	}
}

func evt(op, issue string) proto.Push {
	return proto.Push{Op: proto.EvEvent, Event: &proto.Event{Op: op, Issue: issue, Principal: "bob"}}
}

var refresh = []string{proto.OpReady, proto.OpBlocked, proto.OpClaims, proto.OpList}

// Live watches with events before it reads, then reads the lists once
// for a burst of relevant events, after the burst settles, and not at all
// for a comment. Idle, it calls nothing.
func TestLiveDebounces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := startLive(t, &fakeServer{})
		if got, want := r.srv.took(), append([]string{proto.OpWatch}, refresh...); !slices.Equal(got, want) {
			t.Fatalf("first calls = %v, want %v", got, want)
		}
		if got := r.got(); !slices.Equal(got, []string{"snapshot", "status live"}) {
			t.Fatalf("first updates = %v", got)
		}

		for range 5 {
			r.srv.send(evt("issue.update", "sf-r1"))
			time.Sleep(10 * time.Millisecond)
		}
		synctest.Wait()
		if got := r.srv.took(); len(got) != 0 {
			t.Errorf("read during the burst: %v", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := r.srv.took(); !slices.Equal(got, refresh) {
			t.Errorf("after the burst, calls = %v, want one refresh %v", got, refresh)
		}
		want := []string{"event issue.update", "event issue.update", "event issue.update", "event issue.update", "event issue.update", "snapshot"}
		if got := r.got(); !slices.Equal(got, want) {
			t.Errorf("updates = %v, want %v", got, want)
		}

		r.srv.send(evt("comment.add", "sf-r1"))
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := r.srv.took(); len(got) != 0 {
			t.Errorf("a comment, then ten idle minutes, made calls %v; want none", got)
		}
		if got := r.got(); !slices.Equal(got, []string{"event comment.add"}) {
			t.Errorf("updates = %v, want the comment's event only", got)
		}
	})
}

// After the server's resync Live watches again and rereads; after its own
// queue overflows it rereads.
func TestLiveResyncs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := startLive(t, &fakeServer{})
		r.srv.took()
		r.got()
		r.srv.send(proto.Push{Op: proto.EvResync})
		synctest.Wait()
		if got, want := r.srv.took(), append([]string{proto.OpWatch}, refresh...); !slices.Equal(got, want) {
			t.Errorf("after a resync, calls = %v, want %v", got, want)
		}
		r.got()
		// Live is busy in a refresh while more events come than it holds.
		gate := make(chan struct{})
		r.srv.mu.Lock()
		r.srv.gate = gate
		r.srv.mu.Unlock()
		r.actions <- Action{Do: DoRefresh}
		synctest.Wait()
		for range pushQueue + 10 {
			r.srv.send(evt("comment.add", "sf-r1"))
		}
		r.srv.mu.Lock()
		r.srv.gate = nil
		r.srv.mu.Unlock()
		close(gate)
		synctest.Wait()
		if got, want := r.srv.took(), append(slices.Clone(refresh), refresh...); !slices.Equal(got, want) {
			t.Errorf("after an overflow, calls = %v, want the refresh under way, then another %v", got, want)
		}
	})
}

// A lost connection is reported and redialed, with a growing wait; on
// the new one Live watches and reads afresh.
func TestLiveReconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := startLive(t, &fakeServer{})
		r.srv.took()
		r.got()
		r.srv.last().drop()
		synctest.Wait()
		if got := r.got(); !slices.Equal(got, []string{"status reconnecting"}) {
			t.Fatalf("after the drop, updates = %v", got)
		}
		time.Sleep(retryFirst)
		synctest.Wait()
		if got, want := r.srv.took(), append([]string{proto.OpWatch}, refresh...); !slices.Equal(got, want) {
			t.Errorf("after reconnecting, calls = %v, want %v", got, want)
		}
		if got := r.got(); !slices.Equal(got, []string{"snapshot", "status live"}) {
			t.Errorf("after reconnecting, updates = %v", got)
		}
	})
}

// A refused read leaves the board up with a note, rather than
// reconnecting, which would not help.
func TestLiveRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := &fakeServer{refuse: map[string]*proto.Error{
			proto.OpClaims: proto.Errf(proto.CodeInvalid, "ask the admin to run `starfixd upgrade`", "unknown operation \"claims\"")}}
		r := startLive(t, srv)
		var status Status
		for {
			u, ok := <-r.updates
			if !ok {
				break
			}
			if s, isStatus := u.(Status); isStatus {
				status = s
				break
			}
		}
		if status.State != StateLive || status.Note != "unknown operation \"claims\"; fix: ask the admin to run `starfixd upgrade`" {
			t.Errorf("status = %+v, want live with the refusal and its fix", status)
		}
		if n := len(srv.conns); n != 1 {
			t.Errorf("dialed %d times after a refusal, want once", n)
		}
	})
}

// Show reads an issue's detail; an event on it reads it again with the
// lists; once hidden, it is not read again.
func TestLiveDetail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := startLive(t, &fakeServer{})
		r.srv.took()
		r.got()
		r.actions <- Action{Do: DoShow, ID: "sf-r1"}
		synctest.Wait()
		if got := r.srv.took(); !slices.Equal(got, []string{"show sf-r1"}) {
			t.Errorf("show calls = %v", got)
		}
		r.srv.send(evt("comment.add", "sf-r1"))
		time.Sleep(time.Second)
		synctest.Wait()
		if got := r.srv.took(); !slices.Equal(got, []string{"show sf-r1"}) {
			t.Errorf("a comment on the open issue made calls %v, want its detail read again", got)
		}
		r.actions <- Action{Do: DoHide}
		r.actions <- Action{Do: DoRefresh}
		synctest.Wait()
		if got := r.srv.took(); !slices.Equal(got, refresh) {
			t.Errorf("refresh with no detail open made calls %v, want %v", got, refresh)
		}
		want := []string{"detail sf-r1", "event comment.add", "detail sf-r1", "snapshot"}
		if got := r.got(); !slices.Equal(got, want) {
			t.Errorf("updates = %v, want %v", got, want)
		}
	})
}
