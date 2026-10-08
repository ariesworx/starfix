package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// The inbox, and pushing it (design §7: the "Everything polls" row, and
// the "As built (stage 3, inbox and handoffs)" note). A connection
// that sends watch is pushed, as evt frames, each new item for its
// principal and session. The store hands committed items to the
// connection's watch without blocking; a goroutine per watching
// connection writes them out, and the request loop writes any still
// queued before each response, so an item committed before a request is
// answered reaches the client before the answer.

// pusher writes one watch's items to its connection.
type pusher struct {
	w    *store.Watch
	stop chan struct{}
	done chan struct{}
	// ended: the resync was sent, or a write failed; nothing more is
	// pushed. Guarded by session.pushMu.
	ended bool
}

// watch starts pushing sess's items, or keeps the pushes already running.
// After a resync it starts afresh. The pusher goroutine ends when unwatch
// stops it, a write fails or the resync is sent, and handle's deferred
// unwatch waits for it, so it never outlives the connection.
func (s *Server) watch(ctx context.Context, sess *session, raw json.RawMessage) (any, *proto.Error) {
	var in proto.WatchArgs
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, proto.Errf(proto.CodeInvalid, "upgrade starfix to match the server", fmt.Sprintf("bad arguments: %v", err))
		}
	}
	if p := sess.push; p != nil {
		sess.pushMu.Lock()
		ended := p.ended
		sess.pushMu.Unlock()
		if !ended {
			return s.unread(ctx, sess.actor)
		}
		s.unwatch(sess)
	}
	p := &pusher{w: s.cfg.Store.Watch(sess.actor.Principal, sess.actor.Session),
		stop: make(chan struct{}), done: make(chan struct{})}
	sess.push = p
	go func() {
		defer close(p.done)
		for {
			select {
			case <-p.stop:
				return
			case <-p.w.Ready():
				if !s.flush(sess, p) {
					return
				}
			}
		}
	}()
	// Subscribed first, then counted: nothing falls between the two.
	return s.unread(ctx, sess.actor)
}

// unread answers watch with a's unread count.
func (s *Server) unread(ctx context.Context, a store.Actor) (any, *proto.Error) {
	page, err := s.cfg.Store.Inbox(ctx, a, false, 1)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpWatch, "", 0, err)
	}
	return proto.WatchResult{Unread: page.Unread}, nil
}

// flush writes p's queued items, and the resync if it overflowed. It
// reports whether p still pushes.
func (s *Server) flush(sess *session, p *pusher) bool {
	sess.pushMu.Lock()
	defer sess.pushMu.Unlock()
	if p.ended {
		return false
	}
	items, over := p.w.Take()
	for _, it := range items {
		w := wireInboxItem(it)
		if !s.pushOne(sess, p, proto.Push{Op: proto.EvInbox, Item: &w}) {
			return false
		}
	}
	if over {
		s.pushOne(sess, p, proto.Push{Op: proto.EvResync})
		p.ended = true
		p.w.Close()
		return false
	}
	return true
}

// pushOne writes one event; on failure p ends. sess.pushMu is held.
func (s *Server) pushOne(sess *session, p *pusher, ev proto.Push) bool {
	f, err := ev.Frame()
	if err == nil {
		err = sess.enc.Encode(f)
	}
	if err != nil {
		p.ended = true
		p.w.Close()
		return false
	}
	return true
}

// unwatch stops sess's pushes and waits for its pusher. The connection
// must be closed first if the pusher may be blocked writing to it.
func (s *Server) unwatch(sess *session) {
	p := sess.push
	if p == nil {
		return
	}
	close(p.stop)
	<-p.done
	p.w.Close()
	sess.push = nil
}

func inbox(ctx context.Context, s *Server, a store.Actor, in proto.InboxArgs) (any, *proto.Error) {
	page, err := s.cfg.Store.Inbox(ctx, a, in.All, in.Limit)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpInbox, "", 0, err)
	}
	out := proto.InboxResult{Items: []proto.InboxItem{}, Unread: page.Unread}
	for _, it := range page.Items {
		out.Items = append(out.Items, wireInboxItem(it))
	}
	return out, nil
}

func ack(ctx context.Context, s *Server, a store.Actor, in proto.AckArgs) (any, *proto.Error) {
	n, err := s.cfg.Store.AckInbox(ctx, a, in.IDs, in.All)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpAck, "", 0, err)
	}
	return proto.AckResult{Acked: n}, nil
}

// watchOp answers watch sent where there is no connection to push to.
func watchOp(context.Context, *Server, store.Actor, proto.WatchArgs) (any, *proto.Error) {
	return nil, proto.Errf(proto.CodeInvalid, "send watch on a connection: `sfx watch`",
		"watch pushes to a connection, and this request has none")
}

func wireInboxItem(it store.InboxItem) proto.InboxItem {
	return proto.InboxItem{ID: it.ID, Session: it.Session, Kind: string(it.Kind), Issue: string(it.Issue), Body: it.Body,
		From: it.From, At: it.At.UTC(), ReadAt: it.ReadAt}
}

// wireHandoff is h as viewer reads it. The worktree, a path on the
// author's machine that names their account and layout, goes only to the
// author's own principal (S-14).
func wireHandoff(h store.Handoff, viewer store.Actor) proto.Handoff {
	wt := h.Worktree
	if h.Author != viewer.Principal {
		wt = ""
	}
	return proto.Handoff{Comment: wireComment(h.Comment), HandoffFields: proto.HandoffFields{State: string(h.State),
		Next: h.Next, Branch: h.Branch, Worktree: wt, To: h.To}}
}

// eventState is an event's before or after state as viewer reads it: a
// handoff's worktree in a comment.add event goes only to the author's own
// principal, as in wireHandoff. A state that does not parse, or whose
// handoff does not, is withheld from everyone else: only a corrupt or
// planted row has one, and it may hold the worktree where it cannot be
// taken out.
func eventState(e store.Event, state json.RawMessage, viewer store.Actor) json.RawMessage {
	if e.Op != store.OpCommentAdd || e.Actor.Principal == viewer.Principal || len(state) == 0 {
		return state
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(state, &m) != nil {
		return nil
	}
	raw, ok := m["handoff"]
	if !ok {
		return state // a plain comment
	}
	var h map[string]json.RawMessage // nil for a null handoff
	if json.Unmarshal(raw, &h) != nil {
		return nil
	}
	if h["worktree"] == nil {
		return state
	}
	delete(h, "worktree")
	hb, err := json.Marshal(h)
	if err != nil {
		return nil
	}
	m["handoff"] = hb
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

func storeHandoff(note string, f proto.HandoffFields) store.HandoffNote {
	return store.HandoffNote{Note: note, HandoffFields: store.HandoffFields{State: store.HandoffState(f.State),
		Next: f.Next, Branch: f.Branch, Worktree: f.Worktree, To: f.To}}
}
