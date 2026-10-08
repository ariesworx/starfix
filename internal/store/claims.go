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
// since taken.
//
// A principal may take over its own claim from another session, but only
// when it asks to (StartIssue's take): a person who restarts their agent
// should not wait out the lease, and an agent sharing the person's key
// should not fence the person out by accident. Another principal must wait
// for the lease to expire. Only a claim holds an issue: an issue set
// in_progress without one (at create, by an import, or before claims
// existed) holds nothing, and update cannot set in_progress. The reaper
// (ReapClaims) returns an issue whose lease ran out to open, unassigned,
// if it is still in_progress under the holder's principal.

// Lease bounds. DefaultLease is an agent's lease, renewed while it runs.
// No lease is shorter than MinLease. A start, or a renewal of one
// session's claims, may ask for up to MaxClaimLease; only a renewal of
// every session's claims (`sfx away`) may reach MaxLease.
const (
	DefaultLease  = 15 * time.Minute
	MinLease      = time.Minute
	MaxClaimLease = 24 * time.Hour
	MaxLease      = 7 * 24 * time.Hour
)

// Claim is an issue's current lease: Holder holds it from ClaimedAt
// until ExpiresAt, under Epoch, which rises with each new holder.
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
	// rev is the row's revision, for the compare-and-swap in writeClaim.
	rev int64
	// exists is false for an issue never claimed, which has no row yet.
	exists bool
}

// active reports whether the row holds the issue at now.
func (c claimRow) active(now time.Time) bool {
	return c.Holder.Principal != "" && c.ExpiresAt.After(now)
}

// loadClaim reads the claim on id. An issue never claimed gets a
// claimRow that does not exist yet, with epoch 0 and no holder.
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

// checkEpoch refuses a finish or release naming epoch when epoch is set
// and is not c's current one (*StaleEpochError).
func checkEpoch(c claimRow, epoch int64) error {
	if epoch != 0 && epoch != c.Epoch {
		return &StaleEpochError{ID: c.Issue, Epoch: epoch, Current: c.Epoch, By: c.Holder}
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

// checkLease refuses a lease outside [MinLease, most]; most is
// MaxClaimLease or MaxLease.
func checkLease(d, most time.Duration) error {
	if d < MinLease || d > most {
		return fmt.Errorf("%w: lease must be between %s and %s", ErrInvalid, "1m", leaseText(most))
	}
	return nil
}

// leaseText spells checkLease's upper bound, MaxClaimLease or MaxLease,
// as the refusal shows it.
func leaseText(d time.Duration) string {
	if d == MaxLease {
		return "7d"
	}
	return "24h"
}

// takeClaim leases c to w's actor for lease. A new holder raises the
// epoch, and the holder it replaced gets a claim.lost inbox item; the same
// session taking again keeps the epoch and only extends the lease, which
// is not an event.
func takeClaim(ctx context.Context, w *wtx, c claimRow, lease time.Duration) (Claim, error) {
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

// ClaimOf returns the active claim on an issue, or nil if none is live.
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

// claims reads the claims that where, a constant condition with
// placeholders for args, selects, soonest to expire first.
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

// RenewClaims renews the actor's active claims and returns them all, with
// their new expiry. With allSessions it renews every claim the principal
// holds (a person going away, `sfx away`), for up to MaxLease; otherwise
// only the actor's own session's, for up to MaxClaimLease. A lease out of
// range is refused with ErrInvalid. A claim with less than half of lease
// left is extended to now+lease; one with more is not rewritten, so an
// agent renewing every minute writes about every lease/2, and a lease is
// never shortened. Renewals record no event.
func (s *Store) RenewClaims(ctx context.Context, actor Actor, lease time.Duration, allSessions bool) ([]Claim, error) {
	most := MaxClaimLease
	if allSessions {
		most = MaxLease
	}
	if err := checkLease(lease, most); err != nil {
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
