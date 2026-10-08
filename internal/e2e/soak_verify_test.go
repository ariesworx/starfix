package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ariesworx/starfix/internal/mcpserver"
)

// querier is a *sql.Tx or *sql.DB.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scan runs query and calls row for each result row.
func scan(ctx context.Context, q querier, query string, args []any, row func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", query, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := row(rows); err != nil {
			return fmt.Errorf("%s: %w", query, err)
		}
	}
	return rows.Err()
}

func readEvents(ctx context.Context, q querier, after int64) ([]sevent, error) {
	var out []sevent
	err := scan(ctx, q, `SELECT seq, at, principal, session, op, target, before_state, after_state, COALESCE(idem_key, '')
  FROM events WHERE seq > ? ORDER BY seq`, []any{after}, func(rows *sql.Rows) error {
		var e sevent
		var before, after []byte
		if err := rows.Scan(&e.seq, &e.at, &e.actor.principal, &e.actor.session, &e.op, &e.target, &before, &after, &e.idem); err != nil {
			return err
		}
		e.at = e.at.UTC()
		e.before, e.after = before, after
		out = append(out, e)
		return nil
	})
	return out, err
}

// tIssue is an issues row, as far as the replay rebuilds it.
type tIssue struct {
	title, status, typ, assignee, parent, closeReason string
	priority                                          int
	rev                                               int64
}

// tables are the rows the replay is compared with, from one snapshot.
type tables struct {
	issues map[string]tIssue
	labels map[string]map[string]bool
	deps   map[depKey]bool
	claims map[string]rClaim
	agents map[string]int // registry rows by principal
	unread map[string]int // unread inbox items by principal
	paths  map[string]int // path rows by issue
}

func readTables(ctx context.Context, q querier) (tables, error) {
	tb := tables{issues: map[string]tIssue{}, labels: map[string]map[string]bool{}, deps: map[depKey]bool{},
		claims: map[string]rClaim{}, agents: map[string]int{}, unread: map[string]int{}, paths: map[string]int{}}
	reads := []struct {
		query string
		row   func(*sql.Rows) error
	}{
		{`SELECT id, title, status, priority, type, COALESCE(assignee, ''), COALESCE(parent_id, ''), COALESCE(close_reason, ''), rev FROM issues`,
			func(rows *sql.Rows) error {
				var id string
				var is tIssue
				err := rows.Scan(&id, &is.title, &is.status, &is.priority, &is.typ, &is.assignee, &is.parent, &is.closeReason, &is.rev)
				tb.issues[id] = is
				return err
			}},
		{`SELECT issue_id, label FROM labels`, func(rows *sql.Rows) error {
			var id, l string
			err := rows.Scan(&id, &l)
			if tb.labels[id] == nil {
				tb.labels[id] = map[string]bool{}
			}
			tb.labels[id][l] = true
			return err
		}},
		{`SELECT from_id, to_id, type FROM deps`, func(rows *sql.Rows) error {
			var k depKey
			err := rows.Scan(&k.from, &k.to, &k.typ)
			tb.deps[k] = true
			return err
		}},
		{`SELECT issue_id, COALESCE(principal, ''), COALESCE(session, ''), epoch FROM claims`, func(rows *sql.Rows) error {
			var id string
			var c rClaim
			err := rows.Scan(&id, &c.holder.principal, &c.holder.session, &c.epoch)
			tb.claims[id] = c
			return err
		}},
		{`SELECT principal, COUNT(*) FROM agents GROUP BY principal`, func(rows *sql.Rows) error {
			var p string
			var n int
			err := rows.Scan(&p, &n)
			tb.agents[p] = n
			return err
		}},
		{`SELECT to_principal, COUNT(*) FROM inbox WHERE read_at IS NULL GROUP BY to_principal`, func(rows *sql.Rows) error {
			var p string
			var n int
			err := rows.Scan(&p, &n)
			tb.unread[p] = n
			return err
		}},
		{`SELECT issue_id, COUNT(*) FROM issue_paths GROUP BY issue_id`, func(rows *sql.Rows) error {
			var id string
			var n int
			err := rows.Scan(&id, &n)
			tb.paths[id] = n
			return err
		}},
	}
	for _, rd := range reads {
		if err := scan(ctx, q, rd.query, nil, rd.row); err != nil {
			return tables{}, err
		}
	}
	return tb, nil
}

// snapshot reads the events since the last snapshot and the tables in
// one read-only transaction, replays the events and compares.
func (s *soak) snapshot() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	r := s.rep
	r.mu.Lock()
	defer r.mu.Unlock()
	evs, err := readEvents(ctx, tx, r.seq)
	if err != nil {
		return err
	}
	r.feed(evs)
	r.flush()
	tb, err := readTables(ctx, tx)
	if err != nil {
		return err
	}
	r.compare(tb)
	s.checkCaps(tb)
	r.openCount = 0
	for _, is := range r.issues {
		if is.status != "closed" {
			r.openCount++
		}
	}
	s.stats.snapshots.Add(1)
	return nil
}

// compare checks the replayed state against the tables.
func (r *replay) compare(tb tables) {
	for id, is := range r.issues {
		t, ok := tb.issues[id]
		if !ok {
			r.fail(id, "issue %s is in the event log but not in the issues table", id)
			continue
		}
		want := tIssue{title: is.title, status: is.status, typ: is.typ, assignee: is.assignee, parent: is.parent,
			closeReason: is.closeReason, priority: is.priority, rev: is.rev}
		if t != want {
			r.fail(id, "issue %s: the table has %+v, the event log rebuilds %+v", id, t, want)
		}
		if !maps.Equal(is.labels, tb.labels[id]) {
			r.fail(id, "issue %s: labels %v in the table, %v from the event log", id, slices.Sorted(maps.Keys(tb.labels[id])), slices.Sorted(maps.Keys(is.labels)))
		}
	}
	for id := range tb.issues {
		if _, ok := r.issues[id]; !ok {
			r.fail(id, "issue %s is in the issues table, but no event created it", id)
		}
	}
	for k := range tb.deps {
		if !r.deps[k] {
			r.fail(k.from, "edge %v is in the deps table but not in the event log", k)
		}
	}
	for k := range r.deps {
		if !tb.deps[k] {
			r.fail(k.from, "edge %v is in the event log but not in the deps table", k)
		}
	}
	for id, c := range r.claims {
		t := tb.claims[id]
		if c.epoch == 0 && t.epoch == 0 {
			continue
		}
		switch {
		case t.epoch != c.epoch:
			r.fail(id, "claim on %s: epoch %d in the table, %d from the event log", id, t.epoch, c.epoch)
		case t.holder == c.holder:
		case c.maybeReleased && t.holder == (sessKey{}):
		default:
			r.fail(id, "claim on %s: held by %q in the table, by %q from the event log", id, t.holder, c.holder)
		}
	}
	for id, t := range tb.claims {
		if _, ok := r.claims[id]; !ok && t.epoch > 0 {
			r.fail(id, "claim on %s (epoch %d) is in the table, but no event took it", id, t.epoch)
		}
	}
}

// checkCaps checks that no row count a principal can grow passed its
// limit (rule 18).
func (s *soak) checkCaps(tb tables) {
	l := s.cfg.limits
	for id, ls := range tb.labels {
		if len(ls) > l.Labels {
			s.mon.fail(id, "issue %s has %d labels, past the limit of %d", id, len(ls), l.Labels)
		}
	}
	out := map[string]int{}
	for k := range tb.deps {
		out[k.from]++
	}
	for id, n := range out {
		if n > l.Deps {
			s.mon.fail(id, "issue %s has %d edges out, past the limit of %d", id, n, l.Deps)
		}
	}
	for p, n := range tb.agents {
		if n > l.Sessions {
			s.mon.fail("", "%s has %d registry rows, past the limit of %d", p, n, l.Sessions)
		}
	}
	for p, n := range tb.unread {
		if n > l.InboxUnread {
			s.mon.fail("", "%s has %d unread inbox items, past the limit of %d", p, n, l.InboxUnread)
		}
	}
	for id, n := range tb.paths {
		if n > l.Paths {
			s.mon.fail(id, "issue %s has %d paths, past the limit of %d", id, n, l.Paths)
		}
	}
}

// awaitReaped waits for every lease to lapse and the reaper to end it.
func (s *soak) awaitReaped() {
	deadline := time.Now().Add(s.clock.real(15*time.Minute) + 30*time.Second)
	for {
		var held []string
		err := scan(context.Background(), s.db, `SELECT issue_id FROM claims WHERE principal IS NOT NULL`, nil,
			func(rows *sql.Rows) error {
				var id string
				held = append(held, id)
				return rows.Scan(&held[len(held)-1])
			})
		if err != nil {
			s.t.Errorf("read claims: %v", err)
			return
		}
		if len(held) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, id := range held {
				s.mon.fail(id, "%s is still claimed after every lease ran out: the reaper did not end it", id)
			}
			return
		}
		pause(context.Background(), 50*time.Millisecond)
	}
}

// checkWindow, before a session closes its connection on purpose, waits
// for every item committed so far that its watch is owed.
func (s *soak) checkWindow(ss *session, ln *line, c *mcpserver.RepoConn) {
	var top int64
	err := scan(context.Background(), s.db, `SELECT id FROM inbox ORDER BY id DESC LIMIT 1`, nil,
		func(rows *sql.Rows) error { return rows.Scan(&top) })
	if err != nil {
		s.t.Errorf("read inbox: %v", err)
		return
	}
	s.awaitWindow(ss, ln, c, top)
}

// awaitWindow waits until every inbox item for ss committed after ln's
// window began, up to id upTo (0: all), has been pushed to it: unless the
// window was resynced, or its connection broke, which ends what it is
// owed.
func (s *soak) awaitWindow(ss *session, ln *line, c *mcpserver.RepoConn, upTo int64) {
	w := ln.live()
	if w == nil {
		return
	}
	query := `SELECT id FROM inbox WHERE to_principal = ? AND (to_session IS NULL OR to_session = ?) AND at > ?`
	args := []any{ss.key.principal, ss.key.session, w.start}
	if upTo > 0 {
		query += ` AND id <= ?`
		args = append(args, upTo)
	}
	var owed []int64
	err := scan(context.Background(), s.db, query, args, func(rows *sql.Rows) error {
		var id int64
		owed = append(owed, id)
		return rows.Scan(&owed[len(owed)-1])
	})
	if err != nil {
		s.t.Errorf("read inbox: %v", err)
		return
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		missing, resync := ss.inbox.delivered(w, owed)
		switch {
		case len(missing) == 0 || resync:
			s.stats.windowsChecked.Add(1)
			return
		case c.Err() != nil || ln.live() != w:
			return // the connection broke: what was owed ends with it
		case time.Now().After(deadline):
			s.mon.fail("", "%s: inbox items %v were committed for its live watch (from %s) and never pushed",
				ss.key, missing, stamp(w.start))
			return
		}
		pause(context.Background(), 10*time.Millisecond)
	}
}
