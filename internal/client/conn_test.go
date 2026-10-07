package client

import (
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// fakeServer is the far end of a Conn over net.Pipe.
type fakeServer struct {
	t   *testing.T
	nc  net.Conn
	enc *proto.Encoder
	dec *proto.Decoder
}

// pipeConn returns a Conn, past the handshake, whose pushes go to onPush,
// and the server end it talks to.
func pipeConn(t *testing.T, onPush func(proto.Push)) (*Conn, *fakeServer) {
	t.Helper()
	cli, srv := net.Pipe()
	c := newConn(proto.NewEncoder(cli), proto.NewDecoder(cli), cli.Close, onPush)
	c.run()
	t.Cleanup(func() { _ = c.Close(); _ = srv.Close() })
	return c, &fakeServer{t: t, nc: srv, enc: proto.NewEncoder(srv), dec: proto.NewDecoder(srv)}
}

func (s *fakeServer) req() *proto.Frame {
	s.t.Helper()
	_ = s.nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := s.dec.Decode()
	if err != nil {
		s.t.Errorf("server read: %v", err)
		return nil
	}
	return f
}

func (s *fakeServer) send(f *proto.Frame) {
	s.t.Helper()
	if err := s.enc.Encode(f); err != nil {
		s.t.Errorf("server write: %v", err)
	}
}

func push(t *testing.T, p proto.Push) *proto.Frame {
	t.Helper()
	f, err := p.Frame()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Event frames are delivered to OnPush on the reader goroutine, in order,
// before and between responses and while no call is waiting; unknown
// frame types and event ops are skipped; each call still gets its own
// response.
func TestConnPushes(t *testing.T) {
	var mu sync.Mutex
	var got []string
	arrived := make(chan struct{}, 10)
	c, s := pipeConn(t, func(p proto.Push) {
		mu.Lock()
		defer mu.Unlock()
		name := p.Op
		if p.Item != nil {
			name += ":" + p.Item.Kind
		}
		got = append(got, name)
		arrived <- struct{}{}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		f := s.req()
		if f == nil || f.Op != proto.OpWatch {
			s.t.Errorf("first request %+v, want watch", f)
			return
		}
		s.send(push(t, proto.Push{Op: proto.EvInbox, Item: &proto.InboxItem{ID: 1, Kind: "mention"}}))
		s.send(&proto.Frame{T: "gossip"}) // a frame type from a newer server
		s.send(&proto.Frame{T: proto.FrameEvent, Op: "weather"})
		res, _ := proto.Response(f.ID, proto.WatchResult{Unread: 3}, nil)
		s.send(res)
		// Pushed while the client is idle.
		s.send(push(t, proto.Push{Op: proto.EvInbox, Item: &proto.InboxItem{ID: 2, Kind: "claim.lost"}}))
		s.send(push(t, proto.Push{Op: proto.EvResync}))
	}()
	var w proto.WatchResult
	if err := c.Call(t.Context(), proto.OpWatch, proto.WatchArgs{}, &w); err != nil || w.Unread != 3 {
		t.Fatalf("Call(watch) = %+v, %v; want unread 3", w, err)
	}
	<-done
	for range 4 { // mention, the unknown op, claim.lost, resync
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("pushes did not arrive")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"inbox:mention", "weather", "inbox:claim.lost", "resync"}; !slices.Equal(got, want) {
		t.Fatalf("pushes = %v, want %v", got, want)
	}
}

// A connection that ends while idle reports it: Done closes, Err is set,
// and the next call fails at once.
func TestConnEndsWhileIdle(t *testing.T) {
	c, s := pipeConn(t, nil)
	_ = s.nc.Close()
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done did not close when the server went away")
	}
	if c.Err() == nil {
		t.Error("Err() = nil after the connection ended")
	}
	var pe *proto.Error
	if err := c.Call(t.Context(), proto.OpWho, proto.WhoArgs{}, nil); !errors.As(err, &pe) || pe.Code != proto.CodeUnavailable {
		t.Fatalf("Call after the end = %v, want unavailable", err)
	}
}

// A response that arrives just before the stream ends still answers the
// call waiting for it.
func TestConnResponseBeforeEOF(t *testing.T) {
	c, s := pipeConn(t, nil)
	go func() {
		f := s.req()
		if f == nil {
			return
		}
		res, _ := proto.Response(f.ID, proto.AckResult{Acked: 2}, nil)
		s.send(res)
		_ = s.nc.Close()
	}()
	var r proto.AckResult
	if err := c.Call(t.Context(), proto.OpAck, proto.AckArgs{All: true}, &r); err != nil || r.Acked != 2 {
		t.Fatalf("Call = %+v, %v; want acked 2", r, err)
	}
}
