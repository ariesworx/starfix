package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Token usage (design §12.1). A harness reports what each request cost,
// as counts by model; sfx sends those records here, and AddUsage keeps
// one row per (principal, session, request id), so a batch sent twice is
// stored once. Nothing about an issue is stored with a row: which issue
// it belongs to is worked out when read, from the session's claims
// (attribution.go).

// Granularity says what one usage record covers.
type Granularity string

// Granularities. A request record is one API request, at one instant; a
// turn or session record sums a span, from SpanStart to At.
const (
	GranularityRequest Granularity = "request"
	GranularityTurn    Granularity = "turn"
	GranularitySession Granularity = "session"
)

// OpUsageAdd records one AddUsage call that stored rows. Its after state
// counts what was added and what was already there, and gives the
// earliest and latest record times; the rows themselves are in
// token_usage, so the log does not grow by a row per API request.
const OpUsageAdd Op = "usage.add"

// UsageTarget is the target of usage.add events. It cannot be an issue
// ID, which always has a hyphen; the actor names whose records they were.
const UsageTarget = "usage"

// Usage bounds.
const (
	// MaxUsageCount bounds each count in a record, so sums cannot
	// overflow. A request is far smaller; a whole session's cumulative
	// record fits.
	MaxUsageCount = 1_000_000_000_000
	// UsageSkew is how far past the server's clock a record's time may be.
	UsageSkew = time.Hour
	// MaxUsageSpan bounds a turn's or session's span, so attribution
	// finds the spans that reach into a hold within that much of its end.
	// A longer session reports in parts.
	MaxUsageSpan = 7 * 24 * time.Hour
)

// usageEpoch is the earliest time a record may carry: older is a client
// bug, not a backfill.
var usageEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// Tokens are a record's counts. Nil is unknown, which is not 0.
// CacheWrite1h is the part of CacheWrite written with a one-hour
// lifetime.
type Tokens struct {
	Input        *int64 `json:"input,omitempty"`
	Output       *int64 `json:"output,omitempty"`
	CacheWrite   *int64 `json:"cache_write,omitempty"`
	CacheWrite1h *int64 `json:"cache_write_1h,omitempty"`
	CacheRead    *int64 `json:"cache_read,omitempty"`
}

// UsageRecord is what a harness reported for one request, turn or
// session. At is the time the source gives; SpanStart starts a turn's or
// session's span, and a request has none.
type UsageRecord struct {
	Harness     string
	RequestID   string
	Model       string
	At          time.Time
	Granularity Granularity
	SpanStart   *time.Time
	Tokens
}

// UsageAdded counts the records one AddUsage stored, and those it
// already had.
type UsageAdded struct {
	Added      int `json:"added"`
	Duplicates int `json:"duplicates"`
}

// validate refuses, with ErrInvalid, a record with a field out of
// pattern or range, at the server time now. n numbers it in the message,
// from 1.
func (r UsageRecord) validate(n int, now time.Time) error {
	bad := func(field, rule string) error {
		return fmt.Errorf("%w: usage record %d: %s %s", ErrInvalid, n, field, rule)
	}
	switch {
	case !harnessPattern.MatchString(r.Harness):
		return bad("harness", "must be 1-32 lowercase letters, digits or hyphens")
	case !proto.UsageRequestID.MatchString(r.RequestID):
		return bad("request_id", "must be 1-255 ASCII letters, digits or _.:/+=@-, starting with a letter or digit")
	case !proto.UsageModel.MatchString(r.Model):
		return bad("model", "must be 1-128 ASCII letters, digits or _.:/+@-, starting with a letter or digit")
	case r.At.Before(usageEpoch):
		return bad("at", "must be the request's time, after 2020")
	case r.At.After(now.Add(UsageSkew)):
		return bad("at", fmt.Sprintf("%s is more than an hour past the server's clock", r.At.UTC().Format(time.RFC3339)))
	}
	switch r.Granularity {
	case GranularityRequest:
		if r.SpanStart != nil {
			return bad("span_start", "is for turn and session records; a request has none")
		}
	case GranularityTurn, GranularitySession:
		if s := r.SpanStart; s != nil && (s.After(r.At) || s.Before(usageEpoch)) {
			return bad("span_start", "must be after 2020 and no later than at")
		}
		if s := r.SpanStart; s != nil && r.At.Sub(*s) > MaxUsageSpan {
			return bad("span_start", "must be within 7 days of at; report a longer session in parts")
		}
	default:
		return bad("granularity", "must be request, turn or session")
	}
	for _, c := range []struct {
		name string
		v    *int64
	}{
		{"input", r.Input}, {"output", r.Output}, {"cache_write", r.CacheWrite},
		{"cache_write_1h", r.CacheWrite1h}, {"cache_read", r.CacheRead},
	} {
		if c.v != nil && (*c.v < 0 || *c.v > MaxUsageCount) {
			return bad(c.name, fmt.Sprintf("must be from 0 to %d, or left out when unknown", int64(MaxUsageCount)))
		}
	}
	if h := r.CacheWrite1h; h != nil && (r.CacheWrite == nil || *h > *r.CacheWrite) {
		return bad("cache_write_1h", "is part of cache_write, so needs it and cannot exceed it")
	}
	return nil
}

// usageCols are the token_usage columns AddUsage writes, in order.
const usageCols = `principal, session, request_id, machine, harness, model, at, granularity, span_start,
  input, output, cache_write, cache_write_1h, cache_read, added_at`

// UsageBatchError refuses an AddUsage batch of Got records, more than
// Limits.UsageRecords (Max). It wraps ErrInvalid.
type UsageBatchError struct{ Max, Got int }

func (e *UsageBatchError) Error() string {
	return fmt.Sprintf("%v: at most %d usage records per call, not %d", ErrInvalid, e.Max, e.Got)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *UsageBatchError) Unwrap() error { return ErrInvalid }

// AddUsage stores the actor's usage records and returns how many were
// new and how many it already had: a record whose request id the actor's
// session already reported is ignored, so sending a batch again changes
// nothing. A call that stores rows records one usage.add event. AddUsage
// refuses, storing nothing, a batch of more than Limits.UsageRecords with
// a [*UsageBatchError], one with an invalid record with ErrInvalid, and
// one that would take the principal past Limits.UsagePerDay rows in 24
// hours with ErrBusy.
func (s *Store) AddUsage(ctx context.Context, actor Actor, recs []UsageRecord) (UsageAdded, error) {
	if len(recs) == 0 {
		return UsageAdded{}, nil
	}
	if limit := s.opts.Limits.UsageRecords; len(recs) > limit {
		return UsageAdded{}, &UsageBatchError{Max: limit, Got: len(recs)}
	}
	now := s.now()
	from, to := recs[0].At.UTC(), recs[0].At.UTC()
	for i, r := range recs {
		if err := r.validate(i+1, now); err != nil {
			return UsageAdded{}, err
		}
		from, to = minTime(from, r.At.UTC()), maxTime(to, r.At.UTC())
	}
	var out UsageAdded
	err := s.write(ctx, actor, func(w *wtx) error {
		out = UsageAdded{}
		args := make([]any, 0, len(recs)*15)
		for _, r := range recs {
			var span any
			if r.SpanStart != nil {
				span = r.SpanStart.UTC().Truncate(time.Microsecond)
			}
			args = append(args, w.actor.Principal, w.actor.Session, r.RequestID, w.actor.Machine, r.Harness, r.Model,
				r.At.UTC().Truncate(time.Microsecond), string(r.Granularity), span,
				r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead, w.now)
		}
		row := "(" + placeholders(15) + ")"
		n, err := w.exec(ctx, `INSERT IGNORE INTO token_usage (`+usageCols+`) VALUES `+
			strings.TrimSuffix(strings.Repeat(row+",", len(recs)), ","), args...)
		if err != nil {
			return fmt.Errorf("insert usage: %w", err)
		}
		out = UsageAdded{Added: int(n), Duplicates: len(recs) - int(n)}
		if n == 0 {
			return nil
		}
		if err := w.checkUsageDay(ctx); err != nil {
			return err
		}
		return w.event(ctx, OpUsageAdd, UsageTarget, nil, map[string]any{
			"added": out.Added, "duplicates": out.Duplicates, "from": from, "to": to})
	})
	if err != nil {
		return UsageAdded{}, err
	}
	return out, nil
}

// checkUsageDay refuses, with ErrBusy, a write that left w's principal
// holding more than Limits.UsagePerDay rows added in the last 24 hours.
// It runs after the insert, so the count includes the new rows; the
// refusal rolls them back.
func (w *wtx) checkUsageDay(ctx context.Context) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_usage WHERE principal = ? AND added_at > ?`,
		w.actor.Principal, w.now.Add(-24*time.Hour)).Scan(&n); err != nil {
		return fmt.Errorf("count usage: %w", err)
	}
	if limit := w.lim.UsagePerDay; n > limit {
		return fmt.Errorf("%w: %s may add %d usage records a day, and this batch would make %d", ErrBusy, w.actor.Principal, limit, n)
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
