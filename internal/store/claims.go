package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Claims (design §7). Taking an issue leases it to the actor: the holder
// is the (principal, session, machine) that took it, and the lease runs
// out at ExpiresAt unless renewed. Each take of an issue by a new holder
// raises its epoch, and a finish or release that names an older epoch is
// refused, so a session that lost its claim cannot close work another has
// since taken. The server reaper (ReapClaims) returns expired issues to
// open.
//
// A principal may take over its own claim from another session: a person
// who restarts their agent should not wait out the lease. Another
// principal must wait for the lease to expire.
//
// An issue taken before claims existed, or set in_progress by hand, has
// no claim row; it stays held by its assignee, without a lease, as before.

// Lease bounds. DefaultLease is an agent's lease, renewed while it runs.
const (
	DefaultLease = 15 * time.Minute
	MinLease     = time.Minute
	MaxLease     = 7 * 24 * time.Hour
)

// Claim is an issue's current lease.
type Claim struct {
	Issue     IssueID   `json:"issue"`
	Holder    Actor     `json:"holder"`
	Epoch     int64     `json:"epoch"`
	ClaimedAt time.Time `json:"claimed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Event operations for claims. Renewals are not recorded: an agent renews
// every minute, and the log would be mostly renewals.
const (
	OpClaimTake   Op = "claim.take"
	OpClaimExpire Op = "claim.expire"
)

// ReaperActor is who the reaper's changes are recorded as.
var ReaperActor = Actor{Principal: "starfixd", Session: "reaper", Machine: "server"}

// claimRow is a claims row; Holder.Principal is empty once released.
type claimRow struct {
	Claim
	rev    int64
	exists bool
}

// active reports whether the row holds the issue at now.
func (c claimRow) active(now time.Time) bool {
	return c.Holder.Principal != "" && c.ExpiresAt.After(now)
}

func loadClaim(ctx context.Context, q querier, id IssueID) (claimRow, error) {
	var c claimRow
	var p, sess, m sql.NullString
	var at, exp sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT principal, session, machine, epoch, claimed_at, expires_at, rev
  FROM claims WHERE issue_id = ?`, string(id)).Scan(&p, &sess, &m, &c.Epoch, &at, &exp, &c.rev)
	if errors.Is(err, sql.ErrNoRows) {
		return claimRow{Claim: Claim{Issue: id}}, nil
	}
	if err != nil {
		return claimRow{}, fmt.Errorf("read claim on %s: %w", id, err)
	}
	c.Issue, c.exists = id, true
	c.Holder = Actor{Principal: p.String, Session: sess.String, Machine: m.String}
	c.ClaimedAt, c.ExpiresAt = at.Time.UTC(), exp.Time.UTC()
	return c, nil
}

// heldBy returns the principal holding is at now, or "": the active
// claim's holder, or, for an issue with no claim, the assignee of an
// in_progress issue. An expired claim holds nothing, reaped or not.
func heldBy(c claimRow, is Issue, now time.Time) string {
	switch {
	case c.active(now):
		return c.Holder.Principal
	case c.exists:
		return ""
	case is.Status == StatusInProgress:
		return is.Assignee
	}
	return ""
}

// checkHold refuses actor's finish or release of is: another principal
// holds it (*HeldError), or epoch is set and is not the current one
// (*StaleEpochError).
func checkHold(c claimRow, is Issue, actor Actor, epoch int64, now time.Time) error {
	if by := heldBy(c, is, now); by != "" && by != actor.Principal {
		return &HeldError{ID: is.ID, By: by}
	}
	if epoch != 0 && epoch != c.Epoch {
		return &StaleEpochError{ID: is.ID, Epoch: epoch, Current: c.Epoch, By: c.Holder}
	}
	return nil
}

// writeClaim inserts or updates the claim row. The caller records the
// event, or explicitly writes quietly.
func writeClaim(ctx context.Context, w *wtx, c claimRow) error {
	wid, err := randomInt63()
	if err != nil {
		return err
	}
	var p, sess, m, at, exp any
	if c.Holder.Principal != "" {
		p, sess, m, at, exp = c.Holder.Principal, c.Holder.Session, c.Holder.Machine, c.ClaimedAt, c.ExpiresAt
	}
	if !c.exists {
		_, err = w.exec(ctx, `INSERT INTO claims
  (issue_id, principal, session, machine, epoch, claimed_at, expires_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`, string(c.Issue), p, sess, m, c.Epoch, at, exp, wid)
		if err != nil {
			return fmt.Errorf("insert claim on %s: %w", c.Issue, err)
		}
		return nil
	}
	n, err := w.exec(ctx, `UPDATE claims SET principal = ?, session = ?, machine = ?, epoch = ?,
  claimed_at = ?, expires_at = ?, rev = rev + 1, write_id = ? WHERE issue_id = ? AND rev = ?`,
		p, sess, m, c.Epoch, at, exp, wid, string(c.Issue), c.rev)
	if err != nil {
		return fmt.Errorf("update claim on %s: %w", c.Issue, err)
	}
	if n != 1 {
		return fmt.Errorf("claim on %s changed during update: %w", c.Issue, ErrConflict)
	}
	return nil
}

// releaseClaim drops the holder of an active or expired claim, keeping the
// epoch. It writes nothing when there is no holder.
func releaseClaim(ctx context.Context, w *wtx, c claimRow) error {
	if !c.exists || c.Holder.Principal == "" {
		return nil
	}
	c.Holder = Actor{}
	return writeClaim(ctx, w, c)
}

func checkLease(d time.Duration) error {
	if d < MinLease || d > MaxLease {
		return fmt.Errorf("%w: lease must be between %s and %s", ErrInvalid, MinLease, "7d")
	}
	return nil
}

// take leases c to w's actor for lease. A new holder raises the epoch, and
// the holder it replaced gets a claim.lost inbox item; the same session
// taking again keeps it and only extends the lease.
func take(ctx context.Context, w *wtx, c claimRow, lease time.Duration) (Claim, error) {
	same := c.active(w.now) && c.Holder == w.actor
	if !same && c.Holder.Principal != "" && c.Holder != w.actor {
		// The holder lost it: to another session of its principal, or,
		// once the lease lapsed, to anyone, before the reaper got to it.
		why := "lease expired; "
		if c.active(w.now) {
			why = ""
		}
		if err := w.notify(ctx, InboxItem{To: c.Holder.Principal, Session: c.Holder.Session, Kind: InboxClaimLost, Issue: c.Issue,
			Body: fmt.Sprintf("%staken by %s/%s (epoch %d): stop work on it", why, w.actor.Principal, w.actor.Session, c.Epoch+1)}); err != nil {
			return Claim{}, err
		}
	}
	if !same {
		c.Epoch++
		c.ClaimedAt = w.now
	}
	c.Holder = w.actor
	if exp := w.now.Add(lease); !same || exp.After(c.ExpiresAt) {
		c.ExpiresAt = exp
	}
	if err := writeClaim(ctx, w, c); err != nil {
		return Claim{}, err
	}
	if !same {
		if err := w.event(ctx, OpClaimTake, string(c.Issue), nil,
			map[string]any{"epoch": c.Epoch, "expires_at": c.ExpiresAt}); err != nil {
			return Claim{}, err
		}
	} else {
		w.quiet = true
	}
	return c.Claim, nil
}

// ClaimOf returns the active claim on an issue, or nil if none.
func (s *Store) ClaimOf(ctx context.Context, id IssueID) (*Claim, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	c, err := loadClaim(ctx, s.r, id)
	if err != nil || !c.active(s.now()) {
		return nil, err
	}
	return &c.Claim, nil
}

// ClaimsOf returns the active claims principal holds, in every session,
// soonest to expire first.
func (s *Store) ClaimsOf(ctx context.Context, principal string) ([]Claim, error) {
	return s.claims(ctx, s.r, `principal = ? AND expires_at > ?`, principal, s.now())
}

func (s *Store) claims(ctx context.Context, q querier, where string, args ...any) ([]Claim, error) {
	rows, err := q.QueryContext(ctx, `SELECT issue_id, principal, session, machine, epoch, claimed_at, expires_at
  FROM claims WHERE `+where+` ORDER BY expires_at, issue_id`, args...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Claim{}
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.Issue, &c.Holder.Principal, &c.Holder.Session, &c.Holder.Machine,
			&c.Epoch, &c.ClaimedAt, &c.ExpiresAt); err != nil {
			return nil, fmt.Errorf("claims: %w", err)
		}
		c.ClaimedAt, c.ExpiresAt = c.ClaimedAt.UTC(), c.ExpiresAt.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	return out, nil
}

// RenewClaims extends the actor's active claims to at least now+lease and
// returns them. With allSessions it renews every claim the principal
// holds (a person going away, `sfx away`); otherwise only the actor's own
// session's. A lease is never shortened. A claim with more than half its
// lease left is not rewritten, so an agent renewing every minute writes
// about every lease/2.
func (s *Store) RenewClaims(ctx context.Context, actor Actor, lease time.Duration, allSessions bool) ([]Claim, error) {
	if err := checkLease(lease); err != nil {
		return nil, err
	}
	var out []Claim
	err := s.write(ctx, actor, func(w *wtx) error {
		where, args := `principal = ? AND session = ? AND expires_at > ?`, []any{actor.Principal, actor.Session, w.now}
		if allSessions {
			where, args = `principal = ? AND expires_at > ?`, []any{actor.Principal, w.now}
		}
		held, err := s.claims(ctx, w.tx, where, args...)
		if err != nil {
			return err
		}
		out = held
		for i, c := range held {
			want := w.now.Add(lease)
			if !c.ExpiresAt.Before(want.Add(-lease / 2)) {
				continue
			}
			row, err := loadClaim(ctx, w.tx, c.Issue)
			if err != nil {
				return err
			}
			row.ExpiresAt = want
			if err := writeClaim(ctx, w, row); err != nil {
				return err
			}
			out[i].ExpiresAt = want
			w.quiet = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReapClaims ends every claim whose lease has run out: the claim loses its
// holder, who gets a claim.lost inbox item, and an issue still in_progress
// under the claim's principal goes back to open, unassigned. It returns
// the claims it ended.
func (s *Store) ReapClaims(ctx context.Context) ([]Claim, error) {
	var out []Claim
	err := s.write(ctx, ReaperActor, func(w *wtx) error {
		out = nil
		expired, err := s.claims(ctx, w.tx, `principal IS NOT NULL AND expires_at <= ?`, w.now)
		if err != nil {
			return err
		}
		for _, c := range expired {
			row, err := loadClaim(ctx, w.tx, c.Issue)
			if err != nil {
				return err
			}
			if err := releaseClaim(ctx, w, row); err != nil {
				return err
			}
			if err := w.event(ctx, OpClaimExpire, string(c.Issue),
				map[string]any{"holder": c.Holder, "epoch": c.Epoch, "expires_at": c.ExpiresAt}, nil); err != nil {
				return err
			}
			if err := w.notify(ctx, InboxItem{To: c.Holder.Principal, Session: c.Holder.Session, Kind: InboxClaimLost, Issue: c.Issue,
				Body: fmt.Sprintf("lease expired (epoch %d): stop work on it; start it again if it is still free", c.Epoch)}); err != nil {
				return err
			}
			is, err := loadIssue(ctx, w.tx, c.Issue)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if is.Status == StatusInProgress && is.Assignee == c.Holder.Principal {
				after, err := casUpdate(ctx, w, is, []string{"status = ?", "assignee = ?"}, []any{string(StatusOpen), nil})
				if err != nil {
					return err
				}
				b, a := diff(is, after)
				if err := w.event(ctx, OpIssueUpdate, string(c.Issue), b, a); err != nil {
					return err
				}
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
