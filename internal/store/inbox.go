package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Inbox (design §3, §7): items addressed to a principal, or to one of its
// sessions, about something that happened to them. An item is written in
// the same transaction as the change that causes it, so it exists exactly
// when the change does; items are not events themselves, and acking one
// (marking it read) is not history.
//
// Items are pushed as they are committed to the sessions watching for
// them (Watch), so an agent hears of a lost claim on its next tool call
// rather than when it next polls. A watch made with WatchEvents is also
// pushed every issue event, for a live board.

// InboxKind is what an inbox item is about.
type InboxKind string

// Inbox kinds.
const (
	// InboxClaimLost tells a session it lost its claim: the lease lapsed
	// and was reaped, or another session took the issue over, or closed,
	// finished or released it. It is addressed to that session.
	InboxClaimLost InboxKind = "claim.lost"
	// InboxAssigned tells a principal someone else made it an issue's
	// assignee.
	InboxAssigned InboxKind = "assigned"
	// InboxMention tells a principal someone else wrote @principal in a
	// comment or a handoff note.
	InboxMention InboxKind = "mention"
	// InboxHandoff tells a principal someone else handed it an issue.
	InboxHandoff InboxKind = "handoff"
)

// Inbox bounds.
const (
	// InboxBodyMax is how many bytes of text an item quotes.
	InboxBodyMax = 200
	// DefaultInboxLimit and MaxInboxLimit bound one Inbox read, and
	// MaxInboxLimit one AckInbox by id.
	DefaultInboxLimit = 20
	MaxInboxLimit     = 100
	// MaxMentions bounds the distinct @names one text notifies: the
	// first MaxMentions are looked up, and the rest ignored.
	MaxMentions = 10
	// WatchQueue is how many pushed items a watch holds before it
	// overflows.
	WatchQueue = 64
)

// InboxItem is one item. Session is empty for an item any session of
// the principal may read.
type InboxItem struct {
	ID      int64      `json:"id"`
	To      string     `json:"to"`
	Session string     `json:"session,omitempty"`
	Kind    InboxKind  `json:"kind"`
	Issue   IssueID    `json:"issue,omitempty"`
	Body    string     `json:"body"`
	From    string     `json:"from"`
	At      time.Time  `json:"at"`
	ReadAt  *time.Time `json:"read_at,omitempty"`
}

// PrincipalPattern is what a principal name may look like. starfixd
// refuses a bridge frame naming anything else.
var PrincipalPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// mentionPattern finds @name not preceded by a character that would make
// it part of a word or an email address (no lookbehind in RE2, so the
// preceding character is matched and ignored).
var mentionPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9._@+-])@([a-z][a-z0-9._-]{0,63})`)

// notify records an item in w's transaction and queues it to be pushed
// once the transaction commits. Nothing is written for the actor's own
// principal, unless the item concerns one of its other sessions, and
// nothing past the Notices limit on what the actor sent the recipient in
// the last minute; claim.lost items are exempt from the limit. Recording
// an item may mark the recipient's oldest unread items read (capUnread).
func (w *wtx) notify(ctx context.Context, it InboxItem) error {
	if it.To == w.actor.Principal && (it.Session == "" || it.Session == w.actor.Session) {
		return nil
	}
	if it.Kind != InboxClaimLost {
		var sent int
		if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox
  WHERE to_principal = ? AND from_principal = ? AND kind <> ? AND at > ?`,
			it.To, w.actor.Principal, string(InboxClaimLost), w.now.Add(-time.Minute)).Scan(&sent); err != nil {
			return fmt.Errorf("count notices: %w", err)
		}
		if sent >= w.lim.Notices {
			return nil // over the sender's allowance: not delivered (S-7)
		}
	}
	id, err := lastKey(ctx, w.tx, `SELECT id FROM inbox ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return fmt.Errorf("next inbox id: %w", err)
	}
	wid := randomInt63()
	it.ID, it.From, it.At, it.Body = id+1, w.actor.Principal, w.now, brief(it.Body)
	if _, err := w.exec(ctx, `INSERT INTO inbox
  (id, to_principal, to_session, kind, issue_id, body, from_principal, at, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ID, it.To, nullStr(it.Session), string(it.Kind), nullStr(it.Issue), it.Body, it.From, it.At, wid); err != nil {
		return fmt.Errorf("insert inbox item: %w", err)
	}
	w.inbox = append(w.inbox, it)
	return capUnread(ctx, w, it.To)
}

// capUnread marks principal's oldest unread items read past the
// InboxUnread limit (S-7). They stay under `inbox --all` until Prune
// purges read items.
func capUnread(ctx context.Context, w *wtx, principal string) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox WHERE to_principal = ? AND read_at IS NULL`,
		principal).Scan(&n); err != nil {
		return fmt.Errorf("count unread: %w", err)
	}
	if n <= w.lim.InboxUnread {
		return nil
	}
	wid := randomInt63()
	if _, err := w.exec(ctx, `UPDATE inbox SET read_at = ?, write_id = ? WHERE to_principal = ? AND read_at IS NULL
  ORDER BY id LIMIT ?`, w.now, wid, principal, n-w.lim.InboxUnread); err != nil {
		return fmt.Errorf("cap unread: %w", err)
	}
	return nil
}

// notifyAssigned tells a new assignee, if it is someone else, that they
// have an issue.
func (w *wtx) notifyAssigned(ctx context.Context, is Issue) error {
	if !PrincipalPattern.MatchString(is.Assignee) {
		return nil
	}
	return w.notify(ctx, InboxItem{To: is.Assignee, Kind: InboxAssigned, Issue: is.ID, Body: is.Title})
}

// notifyMentions tells each known principal written as @name in text,
// except those in skip, that they were mentioned. A principal is known
// once it has connected (the agents registry), so an @word that names
// nobody notifies nobody.
func (w *wtx) notifyMentions(ctx context.Context, id IssueID, text string, skip ...string) error {
	var names []any
	for _, m := range mentionPattern.FindAllStringSubmatch(text, -1) {
		name := strings.TrimRight(m[1], "._-")
		if name == "" || slices.Contains(skip, name) || slices.Contains(names, any(name)) {
			continue
		}
		if names = append(names, name); len(names) == MaxMentions {
			break
		}
	}
	if len(names) == 0 {
		return nil
	}
	query := `SELECT DISTINCT principal FROM agents WHERE principal IN (` + placeholders(len(names)) + `) ORDER BY principal` //nolint:gosec // only placeholders are concatenated
	rows, err := w.tx.QueryContext(ctx, query, names...)
	if err != nil {
		return fmt.Errorf("mentions: %w", err)
	}
	var known []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return fmt.Errorf("mentions: %w", err)
		}
		known = append(known, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("mentions: %w", err)
	}
	for _, p := range known {
		if err := w.notify(ctx, InboxItem{To: p, Kind: InboxMention, Issue: id, Body: text}); err != nil {
			return err
		}
	}
	return nil
}

// brief makes text an item body: whitespace and control characters
// collapsed to single spaces, and at most InboxBodyMax bytes of text, cut
// on a rune boundary and followed by "…" when cut.
func brief(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(s) <= InboxBodyMax {
		return s
	}
	n := InboxBodyMax
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// InboxPage is one read of an inbox: items newest first, and how many
// are unread in all.
type InboxPage struct {
	Items  []InboxItem
	Unread int
}

// inboxFor selects the items actor may read: its principal's, and those
// addressed to its own session.
const inboxFor = `to_principal = ? AND (to_session IS NULL OR to_session = ?)`

// Inbox returns actor's unread items (with all, read ones too), newest
// first, at most limit (0 means DefaultInboxLimit), and the unread count,
// from one snapshot. A limit outside 0 to MaxInboxLimit is refused with
// ErrInvalid.
func (s *Store) Inbox(ctx context.Context, actor Actor, all bool, limit int) (InboxPage, error) {
	if err := actor.validate(); err != nil {
		return InboxPage{}, err
	}
	if limit == 0 {
		limit = DefaultInboxLimit
	}
	if limit < 0 || limit > MaxInboxLimit {
		return InboxPage{}, fmt.Errorf("%w: inbox limit must be 1-%d", ErrInvalid, MaxInboxLimit)
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return InboxPage{}, fmt.Errorf("inbox: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep

	page := InboxPage{Items: []InboxItem{}}
	who := []any{actor.Principal, actor.Session}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox WHERE `+inboxFor+` AND read_at IS NULL`, who...).
		Scan(&page.Unread); err != nil {
		return InboxPage{}, fmt.Errorf("inbox: %w", err)
	}
	q := `SELECT id, to_principal, to_session, kind, issue_id, body, from_principal, at, read_at FROM inbox WHERE ` + inboxFor
	if !all {
		q += ` AND read_at IS NULL`
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(who, limit)...)
	if err != nil {
		return InboxPage{}, fmt.Errorf("inbox: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var it InboxItem
		var sess, issue sql.NullString
		var read sql.NullTime
		if err := rows.Scan(&it.ID, &it.To, &sess, &it.Kind, &issue, &it.Body, &it.From, &it.At, &read); err != nil {
			return InboxPage{}, fmt.Errorf("inbox: %w", err)
		}
		it.Session, it.Issue, it.At, it.ReadAt = sess.String, IssueID(issue.String), it.At.UTC(), timePtr(read)
		page.Items = append(page.Items, it)
	}
	if err := rows.Err(); err != nil {
		return InboxPage{}, fmt.Errorf("inbox: %w", err)
	}
	return page, nil
}

// AckInbox marks actor's unread items read: those with the given ids, or
// with all every one it may read. Items actor may not read, and items
// already read, are left alone. It returns how many it marked. Both ids
// and all, neither, more than MaxInboxLimit ids, or an id below 1 is
// refused with ErrInvalid. Acking is not history, so it records no event.
func (s *Store) AckInbox(ctx context.Context, actor Actor, ids []int64, all bool) (int, error) {
	switch {
	case all && len(ids) > 0:
		return 0, fmt.Errorf("%w: ack either ids or all, not both", ErrInvalid)
	case !all && len(ids) == 0:
		return 0, fmt.Errorf("%w: give the ids to ack, or all", ErrInvalid)
	case len(ids) > MaxInboxLimit:
		return 0, fmt.Errorf("%w: ack at most %d ids at once", ErrInvalid, MaxInboxLimit)
	}
	args := []any{}
	for _, id := range ids {
		if id < 1 {
			return 0, fmt.Errorf("%w: inbox id %d", ErrInvalid, id)
		}
		args = append(args, id)
	}
	var n int64
	err := s.write(ctx, actor, func(w *wtx) error {
		wid := randomInt63()
		q := `UPDATE inbox SET read_at = ?, write_id = ? WHERE ` + inboxFor + ` AND read_at IS NULL`
		if !all {
			q += ` AND id IN (` + placeholders(len(args)) + `)`
		}
		var err error
		n, err = w.exec(ctx, q, append([]any{w.now, wid, actor.Principal, actor.Session}, args...)...)
		if err != nil {
			return fmt.Errorf("ack inbox: %w", err)
		}
		w.quiet = true
		return nil
	})
	return int(n), err
}

// Watch is a subscription to the items committed for one principal and
// session from now on: the session's own and the principal's, and, for a
// watch made with WatchEvents, every event committed on an issue. Both
// wait in one queue of WatchQueue; when it is full the watch overflows,
// drops what it held and receives nothing more, so a slow reader never
// holds up the writer. The reader then rereads what it follows and
// watches again. A Watch is safe for concurrent use.
type Watch struct {
	s                  *Store
	principal, session string
	// events: the watch also takes issue events.
	events bool
	// ready holds at most one signal that Take has something to return.
	ready chan struct{}

	// mu guards queue, evq, over and closed. publish takes it while
	// holding the store's watchers.mu, so take watchers.mu first or not
	// at all.
	mu     sync.Mutex
	queue  []InboxItem
	evq    []Event
	over   bool
	closed bool
}

// watchers is the set of open watches.
type watchers struct {
	// mu guards set; it is taken before any Watch's mu.
	mu  sync.Mutex
	set map[*Watch]struct{}
}

// Watch subscribes to principal's items for session. Every item
// committed after Watch returns reaches the watch, so read the inbox
// after subscribing, not before, and nothing falls between the two; an
// item may then arrive both ways. Close the watch when done.
func (s *Store) Watch(principal, session string) *Watch {
	return s.subscribe(&Watch{principal: principal, session: session})
}

// WatchEvents is Watch that also receives every event committed on an
// issue after it returns, without its before and after states (History
// has them). Read what the events change after subscribing, as with the
// inbox.
func (s *Store) WatchEvents(principal, session string) *Watch {
	return s.subscribe(&Watch{principal: principal, session: session, events: true})
}

// subscribe adds w to the store's watches.
func (s *Store) subscribe(w *Watch) *Watch {
	w.s, w.ready = s, make(chan struct{}, 1)
	s.watch.mu.Lock()
	defer s.watch.mu.Unlock()
	if s.watch.set == nil {
		s.watch.set = map[*Watch]struct{}{}
	}
	s.watch.set[w] = struct{}{}
	return w
}

// Ready returns a channel that receives when Take may have something to
// return, an overflow included. Signals merge: one receive may stand for
// many items.
func (w *Watch) Ready() <-chan struct{} { return w.ready }

// Take returns the queued items and events, each oldest first, and
// empties the queue. overflowed reports that the queue filled up: what it
// held was dropped and the watch is finished.
func (w *Watch) Take() (items []InboxItem, events []Event, overflowed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	items, events, w.queue, w.evq = w.queue, w.evq, nil, nil
	return items, events, w.over
}

// Close ends the subscription. It is safe to call more than once.
func (w *Watch) Close() {
	w.s.watch.mu.Lock()
	delete(w.s.watch.set, w)
	w.s.watch.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed, w.queue, w.evq = true, nil, nil
}

// wants reports whether it is addressed to w's principal, and to w's
// session or to every session.
func (w *Watch) wants(it InboxItem) bool {
	return it.To == w.principal && (it.Session == "" || it.Session == w.session)
}

// offer queues what it is given of items and events without blocking,
// and reports whether the watch can take more.
func (w *Watch) offer(items []InboxItem, events []Event) bool {
	var mine []InboxItem
	for _, it := range items {
		if w.wants(it) {
			mine = append(mine, it)
		}
	}
	if !w.events {
		events = nil
	}
	if len(mine)+len(events) == 0 {
		return true
	}
	w.mu.Lock()
	switch {
	case w.closed:
		w.mu.Unlock()
		return false
	case len(w.queue)+len(w.evq)+len(mine)+len(events) > WatchQueue:
		w.over, w.queue, w.evq = true, nil, nil
	default:
		w.queue = append(w.queue, mine...)
		w.evq = append(w.evq, events...)
	}
	over := w.over
	w.mu.Unlock()
	select {
	case w.ready <- struct{}{}:
	default: // already signaled
	}
	return !over
}

// publish hands committed items and issue events to the watches that
// want them. It never blocks: a watch whose queue is full overflows and
// is dropped.
func (s *Store) publish(items []InboxItem, events []Event) {
	s.watch.mu.Lock()
	defer s.watch.mu.Unlock()
	for w := range s.watch.set {
		if !w.offer(items, events) {
			delete(s.watch.set, w)
		}
	}
}
