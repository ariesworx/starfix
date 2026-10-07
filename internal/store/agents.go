package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// Agents registry (design §3, §7): who is at work, on which machine and
// under which harness, and what each session holds. The server touches a
// session's row when it connects and on each claim renewal; Who lists
// the rows seen within a window. Touches are presence, not history, so
// like claim renewals they record no event.

// Registry timing. A row is rewritten at most every AgentTouchEvery
// unless the machine or harness changed; Who's default window is
// AgentActiveFor, and its widest MaxAgentWindow.
const (
	AgentTouchEvery = time.Minute
	AgentActiveFor  = 5 * time.Minute
	MaxAgentWindow  = 7 * 24 * time.Hour
)

// Agent is one session in the registry, with the issues it holds under
// an active claim.
type Agent struct {
	Actor
	Harness  string    `json:"harness,omitempty"`
	Started  time.Time `json:"started"`
	LastSeen time.Time `json:"last_seen"`
	Claims   []IssueID `json:"claims,omitempty"`
}

// harnessPattern is what a harness name may look like: the `sfx setup`
// agent names and others like them.
var harnessPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidHarness reports whether h may be recorded: empty, or 1-32
// lowercase letters, digits and hyphens.
func ValidHarness(h string) bool { return h == "" || harnessPattern.MatchString(h) }

// TouchAgent records that actor's session is present, running under
// harness. It writes only when the row is new, the machine or harness
// changed, or last_seen is AgentTouchEvery old. An empty harness keeps
// the one already recorded, so a client that does not send one does not
// erase it.
func (s *Store) TouchAgent(ctx context.Context, actor Actor, harness string) error {
	if !ValidHarness(harness) {
		return fmt.Errorf("%w: harness %q must be 1-32 lowercase letters, digits or hyphens", ErrInvalid, harness)
	}
	return s.write(ctx, actor, func(w *wtx) error {
		var machine, known string
		var seen time.Time
		var rev int64
		err := w.tx.QueryRowContext(ctx, `SELECT machine, harness, last_seen, rev FROM agents
  WHERE principal = ? AND session = ?`, actor.Principal, actor.Session).Scan(&machine, &known, &seen, &rev)
		if errors.Is(err, sql.ErrNoRows) {
			wid, err := randomInt63()
			if err != nil {
				return err
			}
			if _, err := w.exec(ctx, `INSERT INTO agents
  (principal, session, machine, harness, started, last_seen, rev, write_id) VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
				actor.Principal, actor.Session, actor.Machine, harness, w.now, w.now, wid); err != nil {
				return fmt.Errorf("insert agent %s/%s: %w", actor.Principal, actor.Session, err)
			}
			w.quiet = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("read agent %s/%s: %w", actor.Principal, actor.Session, err)
		}
		if harness == "" {
			harness = known
		}
		if machine == actor.Machine && harness == known && w.now.Sub(seen) < AgentTouchEvery {
			return nil
		}
		wid, err := randomInt63()
		if err != nil {
			return err
		}
		n, err := w.exec(ctx, `UPDATE agents SET machine = ?, harness = ?, last_seen = ?, rev = rev + 1, write_id = ?
  WHERE principal = ? AND session = ? AND rev = ?`,
			actor.Machine, harness, w.now, wid, actor.Principal, actor.Session, rev)
		if err != nil {
			return fmt.Errorf("update agent %s/%s: %w", actor.Principal, actor.Session, err)
		}
		if n != 1 {
			return fmt.Errorf("agent %s/%s changed during update: %w", actor.Principal, actor.Session, ErrConflict)
		}
		w.quiet = true
		return nil
	})
}

// Who returns the sessions seen within since of the server's now (0
// means AgentActiveFor), most recently seen first, each with the issues
// it holds under an active claim.
func (s *Store) Who(ctx context.Context, since time.Duration) ([]Agent, error) {
	if since == 0 {
		since = AgentActiveFor
	}
	if since < 0 || since > MaxAgentWindow {
		return nil, fmt.Errorf("%w: who's window must be up to 7d", ErrInvalid)
	}
	now := s.now()
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("who: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep

	rows, err := tx.QueryContext(ctx, `SELECT principal, session, machine, harness, started, last_seen FROM agents
  WHERE last_seen >= ? ORDER BY last_seen DESC, principal, session`, now.Add(-since))
	if err != nil {
		return nil, fmt.Errorf("who: %w", err)
	}
	out := []Agent{}
	at := map[Actor]int{} // by principal and session only
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Principal, &a.Session, &a.Machine, &a.Harness, &a.Started, &a.LastSeen); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("who: %w", err)
		}
		a.Started, a.LastSeen = a.Started.UTC(), a.LastSeen.UTC()
		at[Actor{Principal: a.Principal, Session: a.Session}] = len(out)
		out = append(out, a)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("who: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	held, err := s.claims(ctx, tx, `principal IS NOT NULL AND expires_at > ?`, now)
	if err != nil {
		return nil, err
	}
	for _, c := range held {
		if i, ok := at[Actor{Principal: c.Holder.Principal, Session: c.Holder.Session}]; ok {
			out[i].Claims = append(out[i].Claims, c.Issue)
		}
	}
	for i := range out {
		slices.Sort(out[i].Claims)
	}
	return out, nil
}
