package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func n64(v int64) *int64 { return &v }

// req is a request record at at with the given input and output counts.
func req(id string, at time.Time, in, out int64) UsageRecord {
	return UsageRecord{Harness: "claude-code", RequestID: id, Model: "claude-opus-4-1", At: at,
		Granularity: GranularityRequest, Input: n64(in), Output: n64(out)}
}

// usageRows counts the token_usage rows principal holds.
func usageRows(t *testing.T, s *Store, principal string) int {
	t.Helper()
	var n int
	if err := s.r.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM token_usage WHERE principal = ?`, principal).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// usageEvents returns the usage.add events after seq.
func usageEvents(t *testing.T, s *Store, after int64) []Event {
	t.Helper()
	evs, err := s.Events(t.Context(), after, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range evs {
		if e.Op == OpUsageAdd {
			out = append(out, e)
		}
	}
	return out
}

func TestAddUsageIgnoresResentRecords(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	now := clk.now()
	seq := lastSeq(t, s)
	batch := []UsageRecord{req("r1", now, 10, 1), req("r2", now, 20, 2)}

	steps := []struct {
		name  string
		actor Actor
		recs  []UsageRecord
		want  UsageAdded
	}{
		{"new batch", alice, batch, UsageAdded{Added: 2}},
		{"same batch again", alice, batch, UsageAdded{Duplicates: 2}},
		{"overlapping batch", alice, []UsageRecord{batch[1], req("r3", now, 5, 5)}, UsageAdded{Added: 1, Duplicates: 1}},
		{"repeat inside one batch", alice, []UsageRecord{req("r4", now, 1, 1), req("r4", now, 1, 1)}, UsageAdded{Added: 1, Duplicates: 1}},
		// Sessions are the client's choice, so another principal's records
		// with the same session and request ids are its own.
		{"another principal", Actor{Principal: "bob", Session: alice.Session, Machine: "m"}, batch, UsageAdded{Added: 2}},
	}
	for _, st := range steps {
		got, err := s.AddUsage(ctx, st.actor, st.recs)
		if err != nil || got != st.want {
			t.Fatalf("%s: AddUsage = %+v, %v; want %+v", st.name, got, err, st.want)
		}
	}
	if n := usageRows(t, s, "alice"); n != 4 {
		t.Errorf("alice has %d usage rows, want 4", n)
	}

	// One summary event per call that added rows, none for the call that
	// added nothing.
	evs := usageEvents(t, s, seq)
	if len(evs) != 4 {
		t.Fatalf("usage.add events = %d, want 4 (one per call that added rows)", len(evs))
	}
	e := evs[0]
	if e.Target != UsageTarget || e.Actor != alice {
		t.Errorf("first event target %q actor %+v, want %q and alice", e.Target, e.Actor, UsageTarget)
	}
	var after struct {
		Added, Duplicates int
		From, To          time.Time
	}
	if err := json.Unmarshal(e.After, &after); err != nil {
		t.Fatal(err)
	}
	if after.Added != 2 || after.Duplicates != 0 || !after.From.Equal(now) || !after.To.Equal(now) {
		t.Errorf("first event after = %s, want added 2, from and to %s", e.After, now)
	}
}

func TestAddUsageEmptyBatchWritesNothing(t *testing.T) {
	s, _ := clockStore(t)
	seq := lastSeq(t, s)
	if got, err := s.AddUsage(t.Context(), alice, nil); err != nil || got != (UsageAdded{}) {
		t.Fatalf("AddUsage(nil) = %+v, %v; want zero, nil", got, err)
	}
	if lastSeq(t, s) != seq {
		t.Error("an empty batch wrote an event")
	}
}

// A write that loses to a concurrent transaction is rerun; the rerun adds
// the rows once and reports them as added, not as duplicates of the
// attempt that rolled back.
func TestAddUsageRerun(t *testing.T) {
	s, clk := clockStore(t)
	seq := lastSeq(t, s)
	attempts := 0
	s.beforeCommit = func(context.Context) error {
		if attempts++; attempts == 1 {
			return errRetry
		}
		return nil
	}
	got, err := s.AddUsage(t.Context(), alice, []UsageRecord{req("r1", clk.now(), 1, 1), req("r2", clk.now(), 1, 1)})
	if err != nil || got != (UsageAdded{Added: 2}) || attempts != 2 {
		t.Fatalf("AddUsage = %+v, %v after %d attempts; want added 2 after 2", got, err, attempts)
	}
	if n := usageRows(t, s, "alice"); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
	if evs := usageEvents(t, s, seq); len(evs) != 1 {
		t.Errorf("usage.add events = %d, want 1", len(evs))
	}
}

func TestAddUsageRefusesInvalidRecords(t *testing.T) {
	s, clk := clockStore(t)
	now := clk.now()
	turn := func(f func(*UsageRecord)) UsageRecord {
		r := req("t1", now, 1, 1)
		r.Granularity = GranularityTurn
		r.SpanStart = ptr(now.Add(-time.Minute))
		f(&r)
		return r
	}
	set := func(f func(*UsageRecord)) UsageRecord {
		r := req("r1", now, 1, 1)
		f(&r)
		return r
	}
	tests := []struct {
		name string
		rec  UsageRecord
		want string // in the message
	}{
		{"no harness", set(func(r *UsageRecord) { r.Harness = "" }), "harness"},
		{"harness in capitals", set(func(r *UsageRecord) { r.Harness = "Claude" }), "harness"},
		{"harness too long", set(func(r *UsageRecord) { r.Harness = strings.Repeat("a", 33) }), "harness"},
		{"no request id", set(func(r *UsageRecord) { r.RequestID = "" }), "request_id"},
		{"request id with a space", set(func(r *UsageRecord) { r.RequestID = "msg 1" }), "request_id"},
		{"request id with an escape", set(func(r *UsageRecord) { r.RequestID = "msg\x1b[2J" }), "request_id"},
		{"request id with bidi", set(func(r *UsageRecord) { r.RequestID = "msg\u202e1" }), "request_id"},
		{"request id like an option", set(func(r *UsageRecord) { r.RequestID = "-rf" }), "request_id"},
		{"request id too long", set(func(r *UsageRecord) { r.RequestID = strings.Repeat("a", 256) }), "request_id"},
		{"no model", set(func(r *UsageRecord) { r.Model = "" }), "model"},
		{"model with a newline", set(func(r *UsageRecord) { r.Model = "gpt\nx" }), "model"},
		{"model too long", set(func(r *UsageRecord) { r.Model = strings.Repeat("m", 129) }), "model"},
		{"no time", set(func(r *UsageRecord) { r.At = time.Time{} }), "at"},
		{"time in the future", set(func(r *UsageRecord) { r.At = now.Add(UsageSkew + time.Second) }), "at"},
		{"time before 2020", set(func(r *UsageRecord) { r.At = time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC) }), "at"},
		{"no granularity", set(func(r *UsageRecord) { r.Granularity = "" }), "granularity"},
		{"unknown granularity", set(func(r *UsageRecord) { r.Granularity = "minute" }), "granularity"},
		{"request with a span", set(func(r *UsageRecord) { r.SpanStart = ptr(now.Add(-time.Second)) }), "span_start"},
		{"span ends before it starts", turn(func(r *UsageRecord) { r.SpanStart = ptr(now.Add(time.Second)) }), "span_start"},
		{"span starts before 2020", turn(func(r *UsageRecord) { r.SpanStart = ptr(time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)) }), "span_start"},
		{"span longer than 7 days", turn(func(r *UsageRecord) { r.SpanStart = ptr(now.Add(-7*24*time.Hour - time.Microsecond)) }), "span_start"},
		{"negative input", set(func(r *UsageRecord) { r.Input = n64(-1) }), "input"},
		{"negative output", set(func(r *UsageRecord) { r.Output = n64(-1) }), "output"},
		{"negative cache write", set(func(r *UsageRecord) { r.CacheWrite = n64(-1) }), "cache_write"},
		{"negative cache read", set(func(r *UsageRecord) { r.CacheRead = n64(-1) }), "cache_read"},
		{"count too large", set(func(r *UsageRecord) { r.CacheRead = n64(MaxUsageCount + 1) }), "cache_read"},
		{"1h cache write without cache write", set(func(r *UsageRecord) { r.CacheWrite1h = n64(1) }), "cache_write_1h"},
		{"1h cache write above cache write", set(func(r *UsageRecord) { r.CacheWrite, r.CacheWrite1h = n64(1), n64(2) }), "cache_write_1h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok := req("ok", now, 1, 1)
			_, err := s.AddUsage(t.Context(), alice, []UsageRecord{ok, tc.rec})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "usage record 2") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AddUsage(%s) = %v; want ErrInvalid naming usage record 2 and %q", tc.name, err, tc.want)
			}
			if n := usageRows(t, s, "alice"); n != 0 {
				t.Errorf("a refused batch stored %d rows", n)
			}
		})
	}
}

func TestAddUsageAcceptsUnknownCountsAndSpans(t *testing.T) {
	s, clk := clockStore(t)
	now := clk.now()
	recs := []UsageRecord{
		{Harness: "gemini", RequestID: "6f1c2a9e-0b7d-4c1e-9a55-1f2e3d4c5b6a", Model: "gemini-2.5-pro", At: now,
			Granularity: GranularityRequest, Input: n64(0), Output: n64(7)},
		{Harness: "cursor", RequestID: "gen_01:req_02", Model: "us.anthropic.claude-sonnet-4@v1/x+y", At: now,
			Granularity: GranularityTurn},
		{Harness: "copilot-cli", RequestID: "session.shutdown=1", Model: "gpt-5", At: now,
			Granularity: GranularitySession, SpanStart: ptr(now.Add(-time.Hour)), CacheWrite: n64(5), CacheWrite1h: n64(5)},
	}
	if got, err := s.AddUsage(t.Context(), alice, recs); err != nil || got.Added != 3 {
		t.Fatalf("AddUsage = %+v, %v; want 3 added", got, err)
	}
	var nulls, zeros int
	if err := s.r.QueryRowContext(t.Context(), `SELECT SUM(input IS NULL), SUM(input = 0) FROM token_usage`).Scan(&nulls, &zeros); err != nil {
		t.Fatal(err)
	}
	if nulls != 2 || zeros != 1 {
		t.Errorf("input: %d NULL and %d zero, want 2 NULL (unknown) and 1 zero", nulls, zeros)
	}
}

// usageStep is one AddUsage call in TestUsageLimits: after advances the
// clock first, and err is what the call must return.
type usageStep struct {
	after time.Duration
	actor Actor
	recs  []UsageRecord
	err   error
}

func TestUsageLimits(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	batch := func(prefix string, n int) []UsageRecord {
		out := make([]UsageRecord, n)
		for i := range out {
			out[i] = req(prefix+strings.Repeat("x", i+1), now, 1, 1)
		}
		return out
	}
	tests := []struct {
		name   string
		limits Limits
		steps  []usageStep
	}{
		{name: "records per call", limits: Limits{UsageRecords: 2}, steps: []usageStep{
			{0, alice, batch("a", 2), nil},
			{0, alice, batch("b", 3), ErrInvalid},
		}},
		{name: "rows per principal per day", limits: Limits{UsagePerDay: 3}, steps: []usageStep{
			{0, alice, batch("a", 2), nil},
			{0, alice, batch("b", 2), ErrBusy},
			{0, alice, batch("a", 2), nil}, // duplicates add nothing, so fit
			{0, bob, batch("a", 3), nil},   // bob has his own allowance
			{0, alice, batch("c", 1), nil},
			{0, alice, batch("d", 1), ErrBusy},
			{24*time.Hour + time.Second, alice, batch("d", 3), nil},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := &clock{t: now}
			s := openStore(t, newDSN(t), Options{Now: clk.now, Limits: tc.limits})
			for i, st := range tc.steps {
				clk.add(st.after)
				before := usageRows(t, s, st.actor.Principal)
				_, err := s.AddUsage(t.Context(), st.actor, st.recs)
				if st.err == nil && err != nil || st.err != nil && !errors.Is(err, st.err) {
					t.Fatalf("step %d: AddUsage(%d records as %s) = %v, want %v", i, len(st.recs), st.actor.Principal, err, st.err)
				}
				if st.err != nil && usageRows(t, s, st.actor.Principal) != before {
					t.Errorf("step %d: a refused batch stored rows", i)
				}
			}
		})
	}
}
