package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// Inbox operations (protocol 2, added before it shipped in a release).
const (
	OpInbox = "inbox" // InboxArgs → InboxResult
	OpAck   = "ack"   // AckArgs → AckResult
	OpWatch = "watch" // WatchArgs → WatchResult; then evt frames
)

// Pushed event ops: the op of an evt frame.
const (
	// EvInbox carries one new inbox item in the frame's e.
	EvInbox = "inbox"
	// EvResync says pushed items were dropped because the client read too
	// slowly: reread the inbox, and send watch again to resume pushes.
	EvResync = "resync"
	// EvEvent carries, to a watch that asked for events, one event
	// committed on an issue: an Event with its Issue and without its
	// before and after states (protocol 3).
	EvEvent = "event"
)

// InboxItem is one inbox item. Kind is claim.lost, assigned, mention or
// handoff. Session is set when the item concerns one session of the
// principal (a lost claim), and empty when any of its sessions may read
// it.
type InboxItem struct {
	ID      int64      `json:"id"`
	Session string     `json:"session,omitempty"`
	Kind    string     `json:"kind"`
	Issue   string     `json:"issue,omitempty"`
	Body    string     `json:"body"`
	From    string     `json:"from"`
	At      time.Time  `json:"at"`
	ReadAt  *time.Time `json:"read_at,omitempty"`
}

// InboxArgs lists the caller's inbox: unread items, or with All read ones
// too, newest first; Limit defaults to 20 and is at most 100.
type InboxArgs struct {
	All   bool `json:"all,omitempty"`
	Limit int  `json:"limit,omitempty"`
}

// InboxResult is a page of items, newest first, and the unread count.
type InboxResult struct {
	Items  []InboxItem `json:"items"`
	Unread int         `json:"unread"`
}

// AckArgs marks items read: those named, or All of the caller's.
type AckArgs struct {
	IDs []int64 `json:"ids,omitempty"`
	All bool    `json:"all,omitempty"`
}

// AckResult says how many items were marked read.
type AckResult struct {
	Acked int `json:"acked"`
}

// WatchArgs asks the server to push this connection's new inbox items:
// its principal's and its own session's, as evt frames, from now until
// the connection closes or a resync. With Events it also pushes every
// event committed on an issue (protocol 3), for a live board. Watching
// again after a resync resumes; watching while watched changes nothing,
// unless Events differs, which starts the watch afresh.
type WatchArgs struct {
	Events bool `json:"events,omitempty"`
}

// WatchResult is the unread count when the watch began.
type WatchResult struct {
	Unread int `json:"unread"`
}

// Push is a server-pushed event: an evt frame decoded. Item is set for
// EvInbox, Event for EvEvent.
type Push struct {
	Op    string
	Item  *InboxItem
	Event *Event
}

// Frame encodes p as an evt frame.
func (p Push) Frame() (*Frame, error) {
	f := &Frame{T: FrameEvent, Op: p.Op}
	var payload any
	switch {
	case p.Item != nil:
		payload = p.Item
	case p.Event != nil:
		payload = p.Event
	default:
		return f, nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode %s event: %w", p.Op, err)
	}
	f.E = b
	return f, nil
}

// DecodePush decodes an evt frame. An inbox or event push without its
// payload is an error; an op this client does not know is returned for
// the caller to ignore.
func DecodePush(f *Frame) (Push, error) {
	if f.T != FrameEvent {
		return Push{}, fmt.Errorf("decode push: %q is not an event frame", f.T)
	}
	p := Push{Op: f.Op}
	var into any
	switch f.Op {
	case EvInbox:
		p.Item = &InboxItem{}
		into = p.Item
	case EvEvent:
		p.Event = &Event{}
		into = p.Event
	default:
		return p, nil
	}
	if len(f.E) == 0 {
		return Push{}, fmt.Errorf("decode push: %s push without its payload", f.Op)
	}
	if err := json.Unmarshal(f.E, into); err != nil {
		return Push{}, fmt.Errorf("decode push: %w", err)
	}
	return p, nil
}

// HandoffFields are a handoff's structured fields: how far the work got
// (State: done, partial or blocked), the next step, the branch and
// worktree it is on, and the principal it is handed to.
type HandoffFields struct {
	State    string `json:"state,omitempty"`
	Next     string `json:"next,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	To       string `json:"to,omitempty"`
}

// Handoff is the latest handoff note on an issue and its fields. It is a
// Comment plus fields, so a protocol 1 client reading start's handoff as
// a Comment still decodes it.
type Handoff struct {
	Comment
	HandoffFields
}
