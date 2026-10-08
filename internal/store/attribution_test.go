package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// tokensText renders a model's counts for comparison: "in/out/cw/cw1h/cr",
// with ? for unknown.
func tokensText(m ModelUsage) string {
	f := func(p *int64) string {
		if p == nil {
			return "?"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("%s %s/%s/%s/%s/%s", m.Model, f(m.Input), f(m.Output), f(m.CacheWrite), f(m.CacheWrite1h), f(m.CacheRead))
}

func modelsText(ms []ModelUsage) string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = tokensText(m)
	}
	return strings.Join(out, "; ")
}

// rec is a request record of model at at, with input in and output out.
func rec(id, model string, at time.Time, in, out int64) UsageRecord {
	r := req(id, at, in, out)
	r.Model = model
	return r
}

// span is a turn record from start to end with input in.
func span(id string, start, end time.Time, in int64) UsageRecord {
	return UsageRecord{Harness: "codex", RequestID: id, Model: "gpt-5", At: end, Granularity: GranularityTurn,
		SpanStart: &start, Tokens: Tokens{Input: n64(in)}}
}

func mustAddUsage(t *testing.T, s *Store, a Actor, recs ...UsageRecord) {
	t.Helper()
	if _, err := s.AddUsage(t.Context(), a, recs); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
}

func mustUsage(t *testing.T, s *Store, id IssueID) IssueUsage {
	t.Helper()
	u, err := s.IssueUsage(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueUsage(%s): %v", id, err)
	}
	return u
}

func checkUsage(t *testing.T, what string, u IssueUsage, held time.Duration, split bool, models string) {
	t.Helper()
	if u.Held != held || u.Split != split || modelsText(u.Models) != models {
		t.Errorf("%s: held %s split %v models %q; want held %s split %v models %q",
			what, u.Held, u.Split, modelsText(u.Models), held, split, models)
	}
}

func TestIssueUsageStartToFinish(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	other := mustCreate(t, s, NewIssue{Title: "untouched"})

	clk.add(time.Minute)
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * time.Minute)
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{}); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	mustAddUsage(t, s, alice,
		rec("before", "opus", t0.Add(30*time.Second), 1000, 1000), // before start: unattributed
		rec("r1", "opus", t0.Add(2*time.Minute), 100, 10),
		rec("r2", "opus", t0.Add(5*time.Minute), 50, 5),
		rec("r3", "sonnet", t0.Add(6*time.Minute), 7, 0),
		rec("after", "opus", t0.Add(11*time.Minute+time.Second), 1000, 1000), // after finish
	)
	mustAddUsage(t, s, bob, rec("bob", "opus", t0.Add(3*time.Minute), 1000, 1000)) // held nothing

	checkUsage(t, "finished issue", mustUsage(t, s, is.ID), 10*time.Minute, false, "opus 150/15/?/?/?; sonnet 7/0/?/?/?")
	checkUsage(t, "issue never taken", mustUsage(t, s, other.ID), 0, false, "")
}

func TestIssueUsageAcrossHandoffAndTakeover(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	is := mustCreate(t, s, NewIssue{Title: "relay"})

	// alice holds it 10m and hands it off; bob holds it 20m; then alice2
	// (another session of alice's) would need bob to finish, so bob
	// finishes; alice takes a second issue over from alice2 by --take.
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * time.Minute)
	if _, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "yours"}, true, ""); err != nil {
		t.Fatal(err)
	}
	clk.add(5 * time.Minute) // nobody holds it
	if _, _, err := s.StartIssue(ctx, bob, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	clk.add(20 * time.Minute)
	if _, _, err := s.FinishIssue(ctx, bob, is.ID, 0, Finish{}); err != nil {
		t.Fatal(err)
	}

	two := mustCreate(t, s, NewIssue{Title: "taken over"})
	t1 := clk.now()
	if _, _, err := s.StartIssue(ctx, alice2, two.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	clk.add(4 * time.Minute)
	if _, _, err := s.StartIssue(ctx, alice, two.ID, 0, true); err != nil {
		t.Fatal(err)
	}
	clk.add(6 * time.Minute)

	mustAddUsage(t, s, alice,
		rec("a1", "opus", t0.Add(5*time.Minute), 10, 1),
		rec("a-gap", "opus", t0.Add(12*time.Minute), 1000, 1000), // released
		rec("a2", "opus", t1.Add(5*time.Minute), 3, 3),           // after the takeover
	)
	mustAddUsage(t, s, bob, rec("b1", "opus", t0.Add(20*time.Minute), 20, 2))
	mustAddUsage(t, s, alice2,
		rec("c1", "opus", t1.Add(time.Minute), 4, 4),
		rec("c-lost", "opus", t1.Add(5*time.Minute), 1000, 1000), // after alice took it over
	)

	checkUsage(t, "handed off", mustUsage(t, s, is.ID), 30*time.Minute, false, "opus 30/3/?/?/?")
	// Still held by alice: time runs to now.
	checkUsage(t, "taken over", mustUsage(t, s, two.ID), 10*time.Minute, false, "opus 7/7/?/?/?")
}

func TestIssueUsageExpiry(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	reaped := mustCreate(t, s, NewIssue{Title: "reaped"})
	lapsed := mustCreate(t, s, NewIssue{Title: "lapsed, not reaped"})
	if _, _, err := s.StartIssue(ctx, alice, reaped.ID, MinLease, false); err != nil {
		t.Fatal(err)
	}
	clk.add(5 * time.Minute)
	if _, err := s.ReapClaims(ctx); err != nil {
		t.Fatal(err)
	}
	t1 := clk.now()
	if _, _, err := s.StartIssue(ctx, bob, lapsed.ID, MinLease, false); err != nil {
		t.Fatal(err)
	}
	clk.add(5 * time.Minute)
	mustAddUsage(t, s, alice,
		rec("in-lease", "opus", t0.Add(30*time.Second), 10, 1),
		rec("past-lease", "opus", t0.Add(3*time.Minute), 1000, 1000))
	mustAddUsage(t, s, bob,
		rec("in-lease", "opus", t1.Add(30*time.Second), 20, 2),
		rec("past-lease", "opus", t1.Add(3*time.Minute), 1000, 1000))

	// The hold ends when the lease ran out, not when the reaper noticed.
	checkUsage(t, "reaped", mustUsage(t, s, reaped.ID), MinLease, false, "opus 10/1/?/?/?")
	checkUsage(t, "lapsed", mustUsage(t, s, lapsed.ID), MinLease, false, "opus 20/2/?/?/?")
}

// One session holds two issues at once: a turn record across both is
// split by time held, and the time no issue was held is unattributed.
func TestIssueUsageSplitAcrossTwoIssues(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	x := mustCreate(t, s, NewIssue{Title: "x"})
	y := mustCreate(t, s, NewIssue{Title: "y"})
	solo := mustCreate(t, s, NewIssue{Title: "solo"})
	step := func(d time.Duration, f func() error) {
		t.Helper()
		clk.add(d)
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	start := func(id IssueID) func() error {
		return func() error { _, _, err := s.StartIssue(ctx, alice, id, time.Hour, false); return err }
	}
	finish := func(id IssueID) func() error {
		return func() error { _, _, err := s.FinishIssue(ctx, alice, id, 0, Finish{}); return err }
	}
	step(0, start(x.ID))              // t0: x
	step(10*time.Minute, start(y.ID)) // t0+10: x and y
	step(10*time.Minute, finish(x.ID))
	step(10*time.Minute, finish(y.ID)) // t0+30
	step(10*time.Minute, start(solo.ID))
	step(10*time.Minute, finish(solo.ID)) // solo held t0+40 to t0+50
	mustAddUsage(t, s, alice,
		// [t0, t0+30]: x alone 10m, both 10m, y alone 10m: x 200+100, y 100+200.
		span("turn", t0, t0.Add(30*time.Minute), 600),
		// [t0-10m, t0+10m]: nothing held 10m (unattributed), x 10m.
		span("early", t0.Add(-10*time.Minute), t0.Add(10*time.Minute), 100),
		// An instant while both are held is shared evenly.
		rec("both", "gpt-5", t0.Add(15*time.Minute), 10, 0),
		// A turn wholly inside solo's hold goes to solo, unsplit.
		span("inside", t0.Add(41*time.Minute), t0.Add(49*time.Minute), 40),
	)
	checkUsage(t, "x", mustUsage(t, s, x.ID), 20*time.Minute, true, "gpt-5 355/0/?/?/?")
	checkUsage(t, "y", mustUsage(t, s, y.ID), 20*time.Minute, true, "gpt-5 305/0/?/?/?")
	checkUsage(t, "solo", mustUsage(t, s, solo.ID), 10*time.Minute, false, "gpt-5 40/?/?/?/?")
}

// Unknown counts stay unknown in a sum; a zero is a known zero; a sum of
// known and unknown is the known part.
func TestIssueUsageUnknownIsNotZero(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	is := mustCreate(t, s, NewIssue{Title: "counts"})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * time.Minute)
	mustAddUsage(t, s, alice,
		UsageRecord{Harness: "gemini", RequestID: "g1", Model: "gemini", At: t0.Add(time.Minute), Granularity: GranularityRequest,
			Tokens: Tokens{Output: n64(0), CacheRead: n64(5)}},
		UsageRecord{Harness: "gemini", RequestID: "g2", Model: "gemini", At: t0.Add(2 * time.Minute), Granularity: GranularityRequest,
			Tokens: Tokens{Output: n64(0)}},
		UsageRecord{Harness: "claude-code", RequestID: "c1", Model: "opus", At: t0.Add(3 * time.Minute), Granularity: GranularityRequest,
			Tokens: Tokens{Input: n64(1), Output: n64(2), CacheWrite: n64(30), CacheWrite1h: n64(10), CacheRead: n64(400)}},
	)
	checkUsage(t, "counts", mustUsage(t, s, is.ID), 10*time.Minute, false, "gemini ?/0/?/?/5; opus 1/2/30/10/400")
}

func TestIssueUsageAccount(t *testing.T) {
	s := newStore(t)
	epic := mustCreate(t, s, NewIssue{Title: "epic", Account: "acme"})
	task := mustCreate(t, s, NewIssue{Title: "task", ParentID: epic.ID})
	u := mustUsage(t, s, task.ID)
	if u.Account != "acme" || u.AccountFrom != epic.ID {
		t.Errorf("IssueUsage(task).Account = %q from %q, want acme from %s", u.Account, u.AccountFrom, epic.ID)
	}
}

func TestDigestUsage(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	w := mustCreate(t, s, NewIssue{Title: "spans the window start"})
	x := mustCreate(t, s, NewIssue{Title: "labeled", Labels: []string{"web"}})
	y := mustCreate(t, s, NewIssue{Title: "still held"})
	step := func(to time.Duration, a Actor, id IssueID, start bool) {
		t.Helper()
		clk.add(at(to).Sub(clk.now()))
		var err error
		if start {
			_, _, err = s.StartIssue(ctx, a, id, time.Hour, false)
		} else {
			_, _, err = s.FinishIssue(ctx, a, id, 0, Finish{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	step(0, alice, w.ID, true)
	step(2*time.Hour, alice, w.ID, false)
	step(2*time.Hour, alice, x.ID, true)
	step(2*time.Hour+10*time.Minute, alice, x.ID, false)
	step(2*time.Hour+30*time.Minute, bob, y.ID, true)
	clk.add(at(3 * time.Hour).Sub(clk.now()))
	mustAddUsage(t, s, alice,
		req("w-old", at(30*time.Minute), 1000, 0), // before the window
		req("w-new", at(90*time.Minute), 10, 0),
		req("x1", at(2*time.Hour+5*time.Minute), 100, 0),
		req("loose", at(2*time.Hour+20*time.Minute), 7, 0)) // nothing held
	mustAddUsage(t, s, bob, req("y1", at(2*time.Hour+40*time.Minute), 50, 0))

	since := at(time.Hour)
	tests := []struct {
		name         string
		f            DigestFilter
		held         time.Duration
		models       string
		unattributed string
	}{
		{"everyone", DigestFilter{Since: since}, time.Hour + 40*time.Minute, "claude-opus-4-1 167/0/?/?/?", "claude-opus-4-1 7/0/?/?/?"},
		{"by alice", DigestFilter{Since: since, By: "alice"}, time.Hour + 10*time.Minute, "claude-opus-4-1 117/0/?/?/?", "claude-opus-4-1 7/0/?/?/?"},
		{"label web", DigestFilter{Since: since, Label: "web"}, 10 * time.Minute, "claude-opus-4-1 100/0/?/?/?", ""},
		{"window with no usage", DigestFilter{Since: at(2*time.Hour + 50*time.Minute), By: "alice"}, 0, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := s.Digest(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			u := d.Usage
			if u.Held != tc.held || modelsText(u.Models) != tc.models || modelsText(u.Unattributed) != tc.unattributed {
				t.Errorf("Digest(%+v).Usage: held %s models %q unattributed %q; want %s, %q, %q",
					tc.f, u.Held, modelsText(u.Models), modelsText(u.Unattributed), tc.held, tc.models, tc.unattributed)
			}
		})
	}
}
