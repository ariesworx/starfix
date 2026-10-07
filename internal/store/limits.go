package store

import (
	"context"
	"fmt"
	"time"
)

// Limits bound what one request, or one principal, can make the store
// hold (S-4, S-6, S-7). Without them a single create with 20,000 labels
// would hold the one writer for 17 s, and a principal could grow the
// registry and another's inbox without end. Zero fields take
// [DefaultLimits]. A request past a limit is refused with ErrInvalid,
// except Sessions, InboxUnread and Notices, which drop rows or notices
// as their fields say.
type Limits struct {
	// Labels caps the labels on one issue, and so in one create.
	Labels int `yaml:"labels_per_issue"`
	// AcceptanceItems caps the items an issue's acceptance text may hold,
	// and the item numbers one accept or finish may name.
	AcceptanceItems int `yaml:"acceptance_items"`
	// Deps caps the edges out of one issue.
	Deps int `yaml:"deps_per_issue"`
	// Sessions caps one principal's rows in the agents registry; a new
	// session past it drops the least recently seen.
	Sessions int `yaml:"sessions_per_principal"`
	// InboxUnread caps one principal's unread items; past it the oldest
	// are marked read, so they stay under `inbox --all` until purged.
	InboxUnread int `yaml:"inbox_unread"`
	// Notices caps the mentions, assignments and handoffs one principal
	// can send another in a minute; the rest are not delivered. Lost
	// claims are not counted.
	Notices int `yaml:"notices_per_minute"`
}

// DefaultLimits are the limits a zero field takes.
var DefaultLimits = Limits{Labels: 50, AcceptanceItems: 200, Deps: 200, Sessions: 256, InboxUnread: 1000, Notices: 10}

// withDefaults returns l with each zero field set from DefaultLimits.
func (l Limits) withDefaults() Limits {
	for _, f := range []struct{ v, d *int }{
		{&l.Labels, &DefaultLimits.Labels}, {&l.AcceptanceItems, &DefaultLimits.AcceptanceItems},
		{&l.Deps, &DefaultLimits.Deps}, {&l.Sessions, &DefaultLimits.Sessions},
		{&l.InboxUnread, &DefaultLimits.InboxUnread}, {&l.Notices, &DefaultLimits.Notices},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	return l
}

// Validate refuses a negative limit with ErrInvalid. Zero is valid: it
// means the default.
func (l Limits) Validate() error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{"labels_per_issue", l.Labels}, {"acceptance_items", l.AcceptanceItems}, {"deps_per_issue", l.Deps},
		{"sessions_per_principal", l.Sessions}, {"inbox_unread", l.InboxUnread}, {"notices_per_minute", l.Notices},
	} {
		if f.v < 0 {
			return fmt.Errorf("%w: limit %s is %d; give a positive number, or leave it out for the default", ErrInvalid, f.name, f.v)
		}
	}
	return nil
}

// Limits returns the limits in force.
func (s *Store) Limits() Limits { return s.opts.Limits }

// Pruned counts what one Prune removed.
type Pruned struct {
	Agents int64
	Inbox  int64
}

// pruneBatch bounds the rows one Prune deletes from each table, so it
// never holds the writer long; the next run takes the rest.
const pruneBatch = 1000

// Prune deletes registry rows not seen within agentKeep, except each
// principal's most recent row (which keeps the principal known to
// mentions), and inbox items read more than inboxKeep ago, at most 1,000
// of each per call. Neither is history, so it records no event. A window
// that is not positive is refused with ErrInvalid. The server's reaper
// runs it.
func (s *Store) Prune(ctx context.Context, agentKeep, inboxKeep time.Duration) (Pruned, error) {
	if agentKeep <= 0 || inboxKeep <= 0 {
		return Pruned{}, fmt.Errorf("%w: prune windows must be positive", ErrInvalid)
	}
	var out Pruned
	err := s.write(ctx, ReaperActor, func(w *wtx) error {
		out = Pruned{}
		w.quiet = true
		rows, err := w.tx.QueryContext(ctx, `SELECT a.principal, a.session FROM agents a
  JOIN (SELECT principal, MAX(last_seen) AS latest FROM agents GROUP BY principal) m ON m.principal = a.principal
  WHERE a.last_seen < ? AND a.last_seen < m.latest
  ORDER BY a.last_seen LIMIT ?`, w.now.Add(-agentKeep), pruneBatch)
		if err != nil {
			return fmt.Errorf("prune agents: %w", err)
		}
		var stale [][2]string
		for rows.Next() {
			var p, sess string
			if err := rows.Scan(&p, &sess); err != nil {
				_ = rows.Close()
				return fmt.Errorf("prune agents: %w", err)
			}
			stale = append(stale, [2]string{p, sess})
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("prune agents: %w", err)
		}
		for _, k := range stale {
			n, err := w.exec(ctx, `DELETE FROM agents WHERE principal = ? AND session = ?`, k[0], k[1])
			if err != nil {
				return fmt.Errorf("prune agents: %w", err)
			}
			out.Agents += n
		}
		n, err := w.exec(ctx, `DELETE FROM inbox WHERE read_at IS NOT NULL AND read_at < ? ORDER BY id LIMIT ?`,
			w.now.Add(-inboxKeep), pruneBatch)
		if err != nil {
			return fmt.Errorf("prune inbox: %w", err)
		}
		out.Inbox = n
		return nil
	})
	return out, err
}
