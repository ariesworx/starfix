package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Human hours (design §12.1). A person logs the time they spent on an
// issue, for a day; reports add it up beside the agents' tokens. Hours
// carry no money, and the account they count against is the issue's,
// resolved when read.

// HoursEntry is one entry: Principal's Duration on Issue on the day On
// (midnight UTC), logged at At.
type HoursEntry struct {
	ID        string        `json:"id"`
	Issue     IssueID       `json:"issue"`
	Principal string        `json:"principal"`
	On        time.Time     `json:"on"`
	Duration  time.Duration `json:"duration"`
	Note      string        `json:"note,omitempty"`
	At        time.Time     `json:"at"`
	// Undone marks an entry a retried log returns that has since been
	// undone.
	Undone bool `json:"-"`
}

// NewHours is an entry to log. On is the logger's calendar day, at most
// a day past the server's today (UTC); a zero On is that today.
// Idem, an idempotency key, makes a retry return the first entry.
type NewHours struct {
	Issue    IssueID
	Duration time.Duration
	On       time.Time
	Note     string
	Idem     string
}

// Hours event ops, each targeting the entry's issue. A log's after state
// and a delete's before state are the entry: its id, day, seconds and
// note, and for a delete whose entry it was.
const (
	OpHoursLog    Op = "hours.log"
	OpHoursDelete Op = "hours.delete"
)

// Hours bounds.
const (
	// MinHoursEntry and MaxHoursEntry bound one entry; a principal's
	// entries on one day also sum to at most MaxHoursEntry.
	MinHoursEntry = time.Minute
	MaxHoursEntry = 24 * time.Hour
	// DefaultHoursList and MaxHoursList bound a listing of entries.
	DefaultHoursList = 50
	MaxHoursList     = 500
)

// hoursIDPattern is an entry id as newCommentID makes one.
var hoursIDPattern = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// HoursLimitError refuses an entry that would give a principal more than
// Limits.HoursPerDay (Max) entries on one day. It wraps ErrInvalid.
type HoursLimitError struct {
	Principal string
	On        time.Time
	Max       int
}

func (e *HoursLimitError) Error() string {
	return fmt.Sprintf("%v: %s has %d entries on %s, the most a day holds", ErrInvalid, e.Principal, e.Max, e.On.Format(time.DateOnly))
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *HoursLimitError) Unwrap() error { return ErrInvalid }

// hoursState is an entry as its events record it.
func hoursState(e HoursEntry) map[string]any {
	return map[string]any{"id": e.ID, "principal": e.Principal, "on": e.On.Format(time.DateOnly),
		"seconds": int64(e.Duration / time.Second), "note": e.Note}
}

// LogHours records the actor's in.Duration on in.Issue for the day
// in.On, and returns the entry. It refuses with ErrNotFound an issue
// that does not exist, and with ErrInvalid a duration under a minute,
// over a day or not in whole seconds, a day that is not a date, is more
// than a day past today (UTC) or is more than a year back, a note longer than
// Limits.HoursNote or not one line of safe text, and an entry that would
// take the actor's day past 24 hours; past Limits.HoursPerDay entries
// that day, it refuses with a [*HoursLimitError].
func (s *Store) LogHours(ctx context.Context, actor Actor, in NewHours) (HoursEntry, error) {
	if err := in.Issue.Validate(); err != nil {
		return HoursEntry{}, err
	}
	now := s.now()
	today := startDay(now)
	on := cmp.Or(in.On, today)
	switch {
	case in.Duration < MinHoursEntry || in.Duration > MaxHoursEntry || in.Duration%time.Second != 0:
		return HoursEntry{}, fmt.Errorf("%w: duration %s must be whole seconds from 1m to 24h", ErrInvalid, in.Duration)
	case !on.Equal(on.UTC().Truncate(24 * time.Hour)):
		return HoursEntry{}, fmt.Errorf("%w: on must be a date (midnight UTC), not %s", ErrInvalid, on.Format(time.RFC3339))
	case on.After(lastDay(now)):
		return HoursEntry{}, fmt.Errorf("%w: on %s is in the future; today is %s (UTC), and a day logged is at most one past it",
			ErrInvalid, on.Format(time.DateOnly), today.Format(time.DateOnly))
	case on.Before(today.AddDate(-1, 0, 0)):
		return HoursEntry{}, fmt.Errorf("%w: on %s is more than a year back", ErrInvalid, on.Format(time.DateOnly))
	}
	if err := checkLine("note", in.Note, s.opts.Limits.HoursNote, false); err != nil {
		return HoursEntry{}, err
	}
	if err := validIdem(in.Idem); err != nil {
		return HoursEntry{}, err
	}
	on = on.UTC()
	var out HoursEntry
	err := s.write(ctx, actor, func(w *wtx) error {
		// The request as sent, with no day for today, so a retry after
		// midnight is the same request.
		done, err := replay(ctx, w, in.Idem, "hours.log", struct {
			Issue   IssueID
			Seconds int64
			On      time.Time
			Note    string
		}{in.Issue, int64(in.Duration / time.Second), in.On.UTC(), in.Note}, &out)
		if err != nil {
			return err
		}
		if done {
			var n int
			if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hours WHERE id = ?`, out.ID).Scan(&n); err != nil {
				return fmt.Errorf("find hours: %w", err)
			}
			out.Undone = n == 0
			return nil
		}
		if err := mustExist(ctx, w.tx, in.Issue); err != nil {
			return err
		}
		var n int
		var sum int64
		if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(seconds), 0) FROM hours WHERE principal = ? AND on_date = ?`,
			w.actor.Principal, on).Scan(&n, &sum); err != nil {
			return fmt.Errorf("count hours: %w", err)
		}
		switch {
		case n >= w.lim.HoursPerDay:
			return &HoursLimitError{Principal: w.actor.Principal, On: on, Max: w.lim.HoursPerDay}
		case time.Duration(sum)*time.Second+in.Duration > MaxHoursEntry:
			return fmt.Errorf("%w: %s has %s on %s already, and a day holds at most 24h", ErrInvalid, w.actor.Principal,
				time.Duration(sum)*time.Second, on.Format(time.DateOnly))
		}
		out = HoursEntry{ID: newCommentID(), Issue: in.Issue, Principal: w.actor.Principal, On: on, Duration: in.Duration,
			Note: in.Note, At: w.now}
		if _, err := w.exec(ctx, `INSERT INTO hours (id, issue_id, principal, on_date, seconds, note, logged_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			out.ID, string(out.Issue), out.Principal, out.On, int64(out.Duration/time.Second), out.Note, out.At); err != nil {
			return fmt.Errorf("log hours: %w", err)
		}
		if err := w.event(ctx, OpHoursLog, string(out.Issue), nil, hoursState(out)); err != nil {
			return err
		}
		return w.settle(out)
	})
	if err != nil {
		return HoursEntry{}, err
	}
	return out, nil
}

// DeleteHours removes the entry id and returns it. Its principal may
// remove it, and an admin may remove anyone's; anyone else is refused
// with a [*ForbiddenError]. A malformed id or idempotency key is
// ErrInvalid, and an entry that is not there ErrNotFound, unless idem
// names the undo that removed it, which returns the entry again.
func (s *Store) DeleteHours(ctx context.Context, actor Actor, id, idem string) (HoursEntry, error) {
	if !hoursIDPattern.MatchString(id) {
		return HoursEntry{}, fmt.Errorf("%w: entry %q must be an hours entry id, as sfx log prints it", ErrInvalid, id)
	}
	if err := validIdem(idem); err != nil {
		return HoursEntry{}, err
	}
	var out HoursEntry
	err := s.write(ctx, actor, func(w *wtx) error {
		if done, err := replay(ctx, w, idem, "hours.delete", id, &out); done || err != nil {
			return err
		}
		rows, err := scanHours(ctx, w.tx, `WHERE id = ?`, []any{id}, 1)
		switch {
		case err != nil:
			return err
		case len(rows) == 0:
			return fmt.Errorf("hours entry %s: %w", id, ErrNotFound)
		case rows[0].Principal != w.actor.Principal && !w.admin:
			return &ForbiddenError{Action: "undoing another person's hours"}
		}
		out = rows[0]
		if _, err := w.exec(ctx, `DELETE FROM hours WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete hours: %w", err)
		}
		if err := w.event(ctx, OpHoursDelete, string(out.Issue), hoursState(out), nil); err != nil {
			return err
		}
		return w.settle(out)
	})
	if err != nil {
		return HoursEntry{}, err
	}
	return out, nil
}

// HoursFilter selects entries: of Issue, logged by Principal, either or
// both; Limit of them (0 takes DefaultHoursList).
type HoursFilter struct {
	Issue     IssueID
	Principal string
	Limit     int
}

// HoursList is a page of entries, newest day first, and how many more
// matched.
type HoursList struct {
	Entries []HoursEntry
	More    int
}

// Hours lists the entries f selects, by day, newest first, then by when
// they were logged. A malformed issue id and a limit out of range are
// ErrInvalid.
func (s *Store) Hours(ctx context.Context, f HoursFilter) (HoursList, error) {
	if f.Limit < 0 || f.Limit > MaxHoursList {
		return HoursList{}, fmt.Errorf("%w: limit must be from 1 to %d, or 0 for %d", ErrInvalid, MaxHoursList, DefaultHoursList)
	}
	where, args := `WHERE TRUE`, []any{}
	if f.Issue != "" {
		if err := f.Issue.Validate(); err != nil {
			return HoursList{}, err
		}
		where, args = where+` AND issue_id = ?`, append(args, string(f.Issue))
	}
	if f.Principal != "" {
		where, args = where+` AND principal = ?`, append(args, f.Principal)
	}
	limit := cmp.Or(f.Limit, DefaultHoursList)
	q, end, err := s.beginRead(ctx)
	if err != nil {
		return HoursList{}, err
	}
	defer end()
	var out HoursList
	if out.Entries, err = scanHours(ctx, q, where+` ORDER BY on_date DESC, logged_at DESC, id`, args, limit); err != nil {
		return HoursList{}, err
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM hours `+where, args...).Scan(&n); err != nil { //nolint:gosec // constant terms; values are arguments
		return HoursList{}, fmt.Errorf("count hours: %w", err)
	}
	out.More = n - len(out.Entries)
	return out, nil
}

// scanHours reads at most limit entries matching where, a constant
// clause with placeholders for args.
func scanHours(ctx context.Context, q querier, where string, args []any, limit int) ([]HoursEntry, error) {
	var out []HoursEntry
	query := `SELECT id, issue_id, principal, on_date, seconds, note, logged_at FROM hours ` + where + ` LIMIT ?` //nolint:gosec // constant terms; values are arguments
	err := scanAll(ctx, q, "hours", query, append(args, limit), func(rs *sql.Rows) error {
		var e HoursEntry
		var secs int64
		if err := rs.Scan(&e.ID, &e.Issue, &e.Principal, &e.On, &secs, &e.Note, &e.At); err != nil {
			return err
		}
		e.On, e.At, e.Duration = e.On.UTC(), e.At.UTC(), time.Duration(secs)*time.Second
		out = append(out, e)
		return nil
	})
	return out, err
}

// PersonHours is the time one principal logged.
type PersonHours struct {
	Principal string
	Duration  time.Duration
}

// issueHours is the time logged on id, in total and by principal, most
// first and then by name.
func issueHours(ctx context.Context, q querier, id IssueID) (time.Duration, []PersonHours, error) {
	var total time.Duration
	var out []PersonHours
	err := scanAll(ctx, q, "issue hours", `SELECT principal, SUM(seconds) FROM hours WHERE issue_id = ? GROUP BY principal`,
		[]any{string(id)}, func(rs *sql.Rows) error {
			var p PersonHours
			var secs int64
			if err := rs.Scan(&p.Principal, &secs); err != nil {
				return err
			}
			p.Duration = time.Duration(secs) * time.Second
			total += p.Duration
			out = append(out, p)
			return nil
		})
	slices.SortFunc(out, func(a, b PersonHours) int {
		return cmp.Or(cmp.Compare(b.Duration, a.Duration), strings.Compare(a.Principal, b.Principal))
	})
	return total, out, err
}

// hoursSum is the time one principal logged on one issue.
type hoursSum struct {
	issue     IssueID
	principal string
	d         time.Duration
}

// startDay is the first day a window starting at since overlaps.
func startDay(since time.Time) time.Time { return since.UTC().Truncate(24 * time.Hour) }

// lastDay is the latest day an entry may be for at now: a logger east of
// UTC may already be on tomorrow.
func lastDay(now time.Time) time.Time { return startDay(now).AddDate(0, 0, 1) }

// hoursEnd is the first day past those a window ending at until counts:
// the days it overlaps, and when it reaches now, every day an entry may
// be for.
func hoursEnd(until, now time.Time) time.Time {
	if until.Before(now) {
		return endDay(until)
	}
	return maxTime(endDay(until), lastDay(now).AddDate(0, 0, 1))
}

// endDay is the first day a window ending before until does not
// overlap.
func endDay(until time.Time) time.Time {
	d := until.UTC().Truncate(24 * time.Hour)
	if d.Before(until) {
		d = d.Add(24 * time.Hour)
	}
	return d
}

// windowHours sums the entries matching where, a constant clause with
// placeholders for args, by issue and principal.
func windowHours(ctx context.Context, q querier, where string, args ...any) ([]hoursSum, error) {
	var out []hoursSum
	query := `SELECT issue_id, principal, SUM(seconds) FROM hours WHERE ` + where + ` GROUP BY issue_id, principal` //nolint:gosec // constant terms; values are arguments
	err := scanAll(ctx, q, "hours", query, args, func(rs *sql.Rows) error {
		var h hoursSum
		var secs int64
		if err := rs.Scan(&h.issue, &h.principal, &secs); err != nil {
			return err
		}
		h.d = time.Duration(secs) * time.Second
		out = append(out, h)
		return nil
	})
	return out, err
}
