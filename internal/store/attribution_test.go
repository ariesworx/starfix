package store

import (
	"fmt"
	"slices"
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
	if _, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "yours"}, true, "", nil); err != nil {
		t.Fatal(err)
	}
	clk.add(5 * time.Minute) // nobody holds it
	// bob's lease outlasts his 20m, as if he renewed: a lapsed lease
	// would end his hold early (TestIssueUsageReleaseThenTake).
	if _, _, err := s.StartIssue(ctx, bob, is.ID, time.Hour, false); err != nil {
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

// A take after the lease lapsed, before the reaper ran, ends the lapsed
// hold at its expiry, which the take's before state records. A take
// recorded before that state existed still ends the hold, at the take.
func TestIssueUsageTakeAfterLapse(t *testing.T) {
	tests := []struct {
		name   string
		legacy bool // the take is recorded as before token capture: no before state
		held   time.Duration
		models string
	}{
		{"before state", false, MinLease + 5*time.Minute, "opus 10/1/?/?/?"},
		{"older event without before state", true, 10 * time.Minute, "opus 1010/1001/?/?/?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := clockStore(t)
			ctx := t.Context()
			t0 := clk.now()
			is := mustCreate(t, s, NewIssue{Title: "lapsed, then taken"})
			if _, _, err := s.StartIssue(ctx, alice, is.ID, MinLease, false); err != nil {
				t.Fatal(err)
			}
			clk.add(5 * time.Minute) // the lease ran out at t0+1m; no reap
			if tc.legacy {
				legacyTake(t, s, bob, is.ID)
			} else if _, _, err := s.StartIssue(ctx, bob, is.ID, time.Hour, false); err != nil {
				t.Fatal(err)
			}
			clk.add(5 * time.Minute)
			mustAddUsage(t, s, alice,
				rec("in-lease", "opus", t0.Add(30*time.Second), 10, 1),
				rec("past-lease", "opus", t0.Add(3*time.Minute), 1000, 1000))
			checkUsage(t, "taken after a lapse", mustUsage(t, s, is.ID), tc.held, false, tc.models)
		})
	}
}

// legacyTake gives id to a as takeClaim did before claim.take recorded
// the claim it replaced.
func legacyTake(t *testing.T, s *Store, a Actor, id IssueID) {
	t.Helper()
	err := s.write(t.Context(), a, func(w *wtx) error {
		c, err := loadClaim(t.Context(), w.tx, id)
		if err != nil {
			return err
		}
		c.Epoch++
		c.Holder, c.ClaimedAt, c.ExpiresAt = a, w.now, w.now.Add(time.Hour)
		if err := writeClaim(t.Context(), w, c); err != nil {
			return err
		}
		return w.event(t.Context(), OpClaimTake, string(id), nil, map[string]any{"epoch": c.Epoch, "expires_at": c.ExpiresAt})
	})
	if err != nil {
		t.Fatalf("legacy take of %s: %v", id, err)
	}
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
			// A lease longer than any hold here, as if renewed: a lapsed
			// lease would end a hold before its finish.
			_, _, err = s.StartIssue(ctx, a, id, 3*time.Hour, false)
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

// A record divided among issues and unheld time divides in whole tokens
// that add back up to it: the issues' parts and the digest's unattributed
// part sum to the digest's total, with nothing lost or invented by
// rounding.
func TestUsagePartsSumToRecord(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	var ids []IssueID
	for _, title := range []string{"x", "y", "z"} {
		is := mustCreate(t, s, NewIssue{Title: title})
		if _, _, err := s.StartIssue(ctx, alice, is.ID, time.Hour, false); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, is.ID)
	}
	clk.add(10 * time.Minute)
	mustAddUsage(t, s, alice,
		// Three ways at an instant: 10 is 4, 3 and 3, not 3 each.
		rec("three", "opus", t0.Add(time.Minute), 10, 1),
		// A third unheld, the rest three ways: 7 is 2 unattributed and
		// 2, 2 and 1, not 2 each.
		UsageRecord{Harness: "codex", RequestID: "turn", Model: "opus", At: t0.Add(2 * time.Minute),
			Granularity: GranularityTurn, SpanStart: ptr(t0.Add(-time.Minute)), Tokens: Tokens{Input: n64(7), Output: n64(5)}},
	)
	d, err := s.Digest(ctx, DigestFilter{Since: t0.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Usage.Models) != 1 || len(d.Usage.Unattributed) != 1 {
		t.Fatalf("Digest usage: models %q unattributed %q; want one model each",
			modelsText(d.Usage.Models), modelsText(d.Usage.Unattributed))
	}
	total, loose := d.Usage.Models[0], d.Usage.Unattributed[0]
	sumIn, sumOut := *loose.Input, *loose.Output
	for _, id := range ids {
		u := mustUsage(t, s, id)
		if len(u.Models) != 1 {
			t.Fatalf("IssueUsage(%s).Models = %q, want one model", id, modelsText(u.Models))
		}
		sumIn += *u.Models[0].Input
		sumOut += *u.Models[0].Output
	}
	if sumIn != *total.Input || sumOut != *total.Output {
		t.Errorf("issues plus unattributed = %d in, %d out; digest total %s; want them equal",
			sumIn, sumOut, tokensText(total))
	}
	if want := "opus 17/6/?/?/?"; tokensText(total) != want {
		t.Errorf("digest total = %q, want %q", tokensText(total), want)
	}
	// Leftover tokens go to the largest remainders, ties in issue order.
	slices.Sort(ids)
	for i, want := range []string{"opus 6/2/?/?/?", "opus 5/1/?/?/?", "opus 4/1/?/?/?"} {
		if got := modelsText(mustUsage(t, s, ids[i]).Models); got != want {
			t.Errorf("IssueUsage(%s, issue %d of 3 by id) = %q, want %q", ids[i], i+1, got, want)
		}
	}
	if want := "opus 2/2/?/?/?"; tokensText(loose) != want {
		t.Errorf("digest unattributed = %q, want %q", tokensText(loose), want)
	}
}

// An issue's usage reads only the records of its own holds, so records
// its sessions made between those holds neither crowd out the ones that
// count nor make the result look capped. A record across two holds
// counts once. Capped is set only when records that count were left out.
func TestIssueUsageReadsOnlyItsHolds(t *testing.T) {
	tests := []struct {
		name   string
		rows   int
		capped bool
		models string
	}{
		{"within the cap", 3, false, "codex-mini 111/?/?/?/?"},
		{"past the cap", 2, true, "codex-mini 110/?/?/?/?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setUsageScanRows(t, tc.rows)
			s, clk := clockStore(t)
			ctx := t.Context()
			t0 := clk.now()
			at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
			x := mustCreate(t, s, NewIssue{Title: "x"})
			other := mustCreate(t, s, NewIssue{Title: "between"})
			step := func(m int, f func() error) {
				t.Helper()
				clk.add(at(m).Sub(clk.now()))
				if err := f(); err != nil {
					t.Fatal(err)
				}
			}
			start := func(id IssueID) func() error {
				return func() error { _, _, err := s.StartIssue(ctx, alice, id, time.Hour, false); return err }
			}
			release := func(id IssueID) func() error {
				return func() error {
					_, err := s.HandoffIssue(ctx, alice, id, 0, HandoffNote{Note: "later"}, true, "", nil)
					return err
				}
			}
			step(0, start(x.ID)) // x held [0, 10m) and [60m, 70m)
			step(10, release(x.ID))
			step(20, start(other.ID))
			step(50, release(other.ID))
			step(60, start(x.ID))
			step(70, release(x.ID))
			recs := []UsageRecord{
				rec("x1", "codex-mini", at(5), 10, 0),
				// 5m of x, 50m of nothing, 5m of x: a sixth is x's.
				{Harness: "codex", RequestID: "across", Model: "codex-mini", At: at(65), Granularity: GranularityTurn,
					SpanStart: ptr(at(5)), Tokens: Tokens{Input: n64(600)}},
				rec("x2", "codex-mini", at(66), 1, 0),
			}
			for i := range 5 { // other's
				recs = append(recs, rec(fmt.Sprintf("o%d", i), "codex-mini", at(30+i), 1000, 0))
			}
			for i := range recs {
				recs[i].Output = nil
			}
			mustAddUsage(t, s, alice, recs...)
			u := mustUsage(t, s, x.ID)
			if modelsText(u.Models) != tc.models || u.Capped != tc.capped {
				t.Errorf("IssueUsage(x) reading at most %d rows = %q capped %v; want %q capped %v",
					tc.rows, modelsText(u.Models), u.Capped, tc.models, tc.capped)
			}
		})
	}
}

// setUsageScanRows bounds the rows one usage read takes, for one test.
func setUsageScanRows(t *testing.T, n int) {
	t.Helper()
	was := usageScanRows
	usageScanRows = n
	t.Cleanup(func() { usageScanRows = was })
}

// Dividing records needs only the holds of their sessions that overlap
// them: not every issue the sessions ever took.
func TestSessionHoldsOverlapTheRecords(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	issue := func(title string) IssueID { return mustCreate(t, s, NewIssue{Title: title}).ID }
	before, open, endsIn, now, bobs := issue("ended before"), issue("open since"), issue("ends inside"), issue("taken now"), issue("bob's")
	step := func(to time.Duration, f func() error) {
		t.Helper()
		clk.add(at(to).Sub(clk.now()))
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	start := func(a Actor, id IssueID) func() error {
		return func() error { _, _, err := s.StartIssue(ctx, a, id, 24*time.Hour, false); return err }
	}
	finish := func(a Actor, id IssueID) func() error {
		return func() error { _, _, err := s.FinishIssue(ctx, a, id, 0, Finish{}); return err }
	}
	step(0, start(alice, before))
	step(time.Minute, start(alice, open))
	step(10*time.Minute, finish(alice, before))
	step(time.Hour, start(alice, endsIn))
	step(2*time.Hour, start(alice, now))
	step(2*time.Hour, start(bob, bobs))
	step(2*time.Hour+10*time.Minute, finish(alice, endsIn))
	rows := []usageRow{
		{key: sessionKey{"alice", "sess-a"}, requestID: "a", from: at(2*time.Hour + 5*time.Minute), to: at(2*time.Hour + 5*time.Minute)},
		{key: sessionKey{"bob", "sess-b"}, requestID: "b", from: at(2*time.Hour + 5*time.Minute), to: at(2*time.Hour + 6*time.Minute)},
	}
	q, end, err := s.beginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	got, err := sessionHolds(ctx, q, rows, clk.now())
	if err != nil {
		t.Fatal(err)
	}
	issues := func(k sessionKey) []IssueID {
		var out []IssueID
		if x := got[k]; x != nil {
			for _, h := range x.holds {
				out = append(out, h.issue)
			}
		}
		slices.Sort(out)
		return out
	}
	want := []IssueID{open, endsIn, now}
	slices.Sort(want)
	if g := issues(rows[0].key); !slices.Equal(g, want) {
		t.Errorf("alice's holds over her record = %v, want %v (not %s, which ended before it)", g, want, before)
	}
	if g := issues(rows[1].key); !slices.Equal(g, []IssueID{bobs}) {
		t.Errorf("bob's holds over his record = %v, want [%s]", g, bobs)
	}
}
