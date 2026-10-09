package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Subscription plans (design §12.1). An admin records what a flat-rate
// plan costs a month and whose usage it pays for; a cost report splits
// each month's fee across the issues those principals' tokens went to
// (cost.go), beside the list-price equivalent, never instead of it.

// NewPlan is a plan's terms from the month From (its first day, UTC) on,
// until a later From of the same name: Fee micro-dollars (10⁻⁶ USD) per
// seat per month, Seats seats, and the Principals whose usage it covers.
// No fee or no seats ends the plan.
type NewPlan struct {
	Name       string
	From       time.Time
	Fee        int64
	Seats      int
	Principals []string
}

// Plan is a plan's terms from a month on, its principals sorted, and who
// last set them.
type Plan struct {
	NewPlan
	SetBy string
	SetAt time.Time
}

// PlanChange says what a SetPlan did.
type PlanChange string

// The changes SetPlan reports.
const (
	PlanAdded     PlanChange = "added"
	PlanReplaced  PlanChange = "replaced"
	PlanUnchanged PlanChange = "unchanged"
)

// OpPlanSet records a plan's terms added or replaced. Its after state is
// the name, the month (2006-01), the fee, the seats and the principals;
// a replace's before state has the terms replaced.
const OpPlanSet Op = "plan.set"

// PlanTarget is the target of plan.set events. A plan's name can look
// like an issue ID, so the name is in the states, not the target.
const PlanTarget = "plans"

// Plan bounds.
const (
	// MaxPlanFee bounds a fee: a million US dollars a seat a month.
	MaxPlanFee = 1_000_000_000_000
	// MaxPlanSeats bounds a plan's seats.
	MaxPlanSeats = 100_000
	// PlanLead is how far ahead a plan's terms may take effect.
	PlanLead = PriceLead
)

// PlanLimitError refuses a new plan row when the table holds Limits.Plans
// (Max) rows. It wraps ErrInvalid.
type PlanLimitError struct{ Max int }

func (e *PlanLimitError) Error() string {
	return fmt.Sprintf("%v: the server keeps at most %d plan rows", ErrInvalid, e.Max)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *PlanLimitError) Unwrap() error { return ErrInvalid }

// planState is a plan's terms as its events record them.
func planState(p NewPlan) map[string]any {
	return map[string]any{"name": p.Name, "from": p.From.Format("2006-01"), "fee": p.Fee, "seats": p.Seats,
		"principals": p.Principals}
}

// SetPlan records a plan's terms from p.From on and says whether it added
// them, replaced those with the same name and month, or found them
// already so. A change records a plan.set event; an unchanged plan writes
// nothing. Only an admin may set a plan: anyone else is refused with a
// [*ForbiddenError]. A name that is not a code name of 1-64 bytes, a
// From that is not the first of a month, is before 2020 or is more than
// PlanLead ahead, a fee or seats out of range, and a principal that is
// not a principal name or past Limits.PlanPrincipals are refused with
// ErrInvalid, and a new row past Limits.Plans with a [*PlanLimitError].
func (s *Store) SetPlan(ctx context.Context, actor Actor, p NewPlan) (PlanChange, error) {
	if !s.IsAdmin(actor.Principal) {
		return "", &ForbiddenError{Action: "plans set"}
	}
	from := p.From.UTC()
	switch now := s.now(); {
	case !ValidAccount(p.Name):
		return "", fmt.Errorf("%w: name %q must be 1-64 lowercase letters and digits, in runs joined by single hyphens", ErrInvalid, p.Name)
	case from.Day() != 1 || !from.Equal(from.Truncate(24*time.Hour)):
		return "", fmt.Errorf("%w: from must be a month (2006-01), not %s", ErrInvalid, from.Format(time.RFC3339))
	case from.Before(usageEpoch):
		return "", fmt.Errorf("%w: from must be %s or later", ErrInvalid, usageEpoch.Format("2006-01"))
	case from.After(now.Add(PlanLead)):
		return "", fmt.Errorf("%w: from %s is more than a year ahead", ErrInvalid, from.Format("2006-01"))
	case p.Fee < 0 || p.Fee > MaxPlanFee:
		return "", fmt.Errorf("%w: fee must be from 0 to %d micro-dollars a seat a month", ErrInvalid, int64(MaxPlanFee))
	case p.Seats < 0 || p.Seats > MaxPlanSeats:
		return "", fmt.Errorf("%w: seats must be from 0 to %d", ErrInvalid, MaxPlanSeats)
	}
	principals := slices.Compact(slices.Sorted(slices.Values(p.Principals)))
	for _, pr := range principals {
		if !PrincipalPattern.MatchString(pr) {
			return "", fmt.Errorf("%w: principal %q must be lowercase letters, digits and ._-, starting with a letter", ErrInvalid, pr)
		}
	}
	if limit := s.opts.Limits.PlanPrincipals; len(principals) > limit {
		return "", fmt.Errorf("%w: a plan names at most %d principals (limits: plan_principals), not %d", ErrInvalid, limit, len(principals))
	}
	p = NewPlan{Name: p.Name, From: from, Fee: p.Fee, Seats: p.Seats, Principals: principals}
	if p.Principals == nil {
		p.Principals = []string{}
	}
	var out PlanChange
	err := s.write(ctx, actor, func(w *wtx) error {
		out = ""
		cur := NewPlan{Name: p.Name, From: from}
		err := w.tx.QueryRowContext(ctx, `SELECT fee, seats FROM plans WHERE name = ? AND from_month = ?`, p.Name, from).
			Scan(&cur.Fee, &cur.Seats)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read plan: %w", err)
		}
		if exists {
			if cur.Principals, err = planPrincipals(ctx, w.tx, p.Name, from); err != nil {
				return err
			}
			if cur.Fee == p.Fee && cur.Seats == p.Seats && slices.Equal(cur.Principals, p.Principals) {
				out = PlanUnchanged
				return nil
			}
			if _, err := w.exec(ctx, `UPDATE plans SET fee = ?, seats = ?, set_by = ?, set_at = ?, write_id = ? WHERE name = ? AND from_month = ?`,
				p.Fee, p.Seats, w.actor.Principal, w.now, randomInt63(), p.Name, from); err != nil {
				return fmt.Errorf("replace plan: %w", err)
			}
			if _, err := w.exec(ctx, `DELETE FROM plan_principals WHERE name = ? AND from_month = ?`, p.Name, from); err != nil {
				return fmt.Errorf("replace plan: %w", err)
			}
			out = PlanReplaced
		} else {
			var n int
			if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans`).Scan(&n); err != nil {
				return fmt.Errorf("count plans: %w", err)
			}
			if n >= w.lim.Plans {
				return &PlanLimitError{Max: w.lim.Plans}
			}
			if _, err := w.exec(ctx, `INSERT INTO plans (name, from_month, fee, seats, set_by, set_at, write_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				p.Name, from, p.Fee, p.Seats, w.actor.Principal, w.now, randomInt63()); err != nil {
				return fmt.Errorf("add plan: %w", err)
			}
			out = PlanAdded
		}
		for _, pr := range p.Principals {
			if _, err := w.exec(ctx, `INSERT INTO plan_principals (name, from_month, principal) VALUES (?, ?, ?)`, p.Name, from, pr); err != nil {
				return fmt.Errorf("plan principals: %w", err)
			}
		}
		var before any
		if exists {
			before = planState(cur)
		}
		return w.event(ctx, OpPlanSet, PlanTarget, before, planState(p))
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// planPrincipals reads the principals of one plan row, sorted.
func planPrincipals(ctx context.Context, q querier, name string, from time.Time) ([]string, error) {
	out := []string{}
	err := scanAll(ctx, q, "plan principals", `SELECT principal FROM plan_principals WHERE name = ? AND from_month = ? ORDER BY principal`,
		[]any{name, from}, func(rs *sql.Rows) error {
			var p string
			if err := rs.Scan(&p); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	return out, err
}

// Plans lists every plan row, by name and then month. The table holds at
// most Limits.Plans rows.
func (s *Store) Plans(ctx context.Context) ([]Plan, error) {
	q, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer end()
	return loadPlans(ctx, q)
}

// loadPlans reads every plan row on q with its principals, by name and
// then month.
func loadPlans(ctx context.Context, q querier) ([]Plan, error) {
	var out []Plan
	err := scanAll(ctx, q, "plans", `SELECT name, from_month, fee, seats, set_by, set_at FROM plans ORDER BY name, from_month`, nil,
		func(rs *sql.Rows) error {
			var p Plan
			if err := rs.Scan(&p.Name, &p.From, &p.Fee, &p.Seats, &p.SetBy, &p.SetAt); err != nil {
				return err
			}
			p.From, p.SetAt, p.Principals = p.From.UTC(), p.SetAt.UTC(), []string{}
			out = append(out, p)
			return nil
		})
	if err != nil {
		return nil, err
	}
	type key struct {
		name string
		from time.Time
	}
	at := map[key]int{}
	for i, p := range out {
		at[key{p.Name, p.From}] = i
	}
	err = scanAll(ctx, q, "plan principals", `SELECT name, from_month, principal FROM plan_principals ORDER BY name, from_month, principal`, nil,
		func(rs *sql.Rows) error {
			var k key
			var pr string
			if err := rs.Scan(&k.name, &k.from, &pr); err != nil {
				return err
			}
			if i, ok := at[key{k.name, k.from.UTC()}]; ok {
				out[i].Principals = append(out[i].Principals, pr)
			}
			return nil
		})
	return out, err
}
