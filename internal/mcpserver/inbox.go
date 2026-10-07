package mcpserver

import (
	"context"
	"fmt"
	"sync"

	"github.com/ariesworx/starfix/internal/proto"
)

// The inbox in an agent session. sfx mcp watches on every connection it
// opens, so the server pushes the session's new inbox items as they
// happen. It keeps count, and the next tool result carries one extra
// line, "inbox: N new (call inbox)": the agent hears of a lost claim or a
// handoff without spending a turn to poll for it.

// pushed counts what the server pushed since the agent was last told.
type pushed struct {
	mu     sync.Mutex
	n      int
	resync bool
}

// Push takes one event the server pushed. It runs on the connection's
// reader goroutine, so it only counts: it must not block or call the
// connection.
func (s *Server) Push(p proto.Push) {
	s.pushed.mu.Lock()
	defer s.pushed.mu.Unlock()
	switch p.Op {
	case proto.EvInbox:
		s.pushed.n++
	case proto.EvResync:
		// Pushes stopped: some items were not counted, and the next
		// call watches again.
		s.pushed.resync = true
		s.link.rewatch.Store(true)
	}
}

// notice returns the line for the next tool result, or "", and resets
// the count.
func (s *Server) notice() string {
	s.pushed.mu.Lock()
	defer s.pushed.mu.Unlock()
	n, resync := s.pushed.n, s.pushed.resync
	s.pushed.n, s.pushed.resync = 0, false
	switch {
	case resync:
		return "inbox: new items (call inbox)"
	case n > 0:
		return fmt.Sprintf("inbox: %d new (call inbox)", n)
	}
	return ""
}

// watch asks c's server to push this session's inbox. A server without
// the inbox refuses, and there is nothing to push; a dropped connection
// shows up in the call that follows.
func watch(ctx context.Context, c Conn) {
	_ = c.Call(ctx, proto.OpWatch, proto.WatchArgs{}, nil)
}

// InboxIn reads the inbox, after acking.
type InboxIn struct {
	Ack []int64 `json:"ack,omitempty"` // ids to mark read first
}

// InboxItem is one item, compact.
type InboxItem struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Issue string `json:"issue,omitempty"`
	From  string `json:"from"`
	At    string `json:"at"`
	Body  string `json:"body"`
}

// Inbox is the unread items, newest first. More says some were left out.
type Inbox struct {
	Items  []InboxItem `json:"items"`
	Unread int         `json:"unread"`
	Acked  int         `json:"acked,omitempty"`
	More   bool        `json:"more,omitempty"`
}

// inboxLimit is how many items the inbox tool asks for.
const inboxLimit = 20

func inbox(ctx context.Context, c Conn, in InboxIn) (Inbox, error) {
	var out Inbox
	if len(in.Ack) > 0 {
		var r proto.AckResult
		if err := c.Call(ctx, proto.OpAck, proto.AckArgs{IDs: in.Ack}, &r); err != nil {
			return Inbox{}, err
		}
		out.Acked = r.Acked
	}
	var r proto.InboxResult
	if err := c.Call(ctx, proto.OpInbox, proto.InboxArgs{Limit: inboxLimit}, &r); err != nil {
		return Inbox{}, err
	}
	out.Items, out.Unread = compactItems(r.Items), r.Unread
	out.More = len(out.Items) < r.Unread
	for size(out) > MaxResultTokens && len(out.Items) > 1 {
		out.Items, out.More = out.Items[:len(out.Items)-1], true
	}
	return out, nil
}

func compactItems(items []proto.InboxItem) []InboxItem {
	out := []InboxItem{}
	for _, it := range items {
		out = append(out, InboxItem{ID: it.ID, Kind: it.Kind, Issue: it.Issue, From: it.From, At: stamp(it.At), Body: it.Body})
	}
	return out
}
