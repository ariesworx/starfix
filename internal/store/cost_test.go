package store

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"
)

// picos is a cost's picodollars as text, "0" for none.
func picos(c Cost) string {
	if c.Picodollars == nil {
		return "0"
	}
	return c.Picodollars.String()
}

// cacheRec is a request record of model at at with every count.
func cacheRec(id, model string, at time.Time, in, out, cw int64, cw1h *int64, cr int64) UsageRecord {
	r := rec(id, model, at, in, out)
	r.CacheWrite, r.CacheWrite1h, r.CacheRead = n64(cw), cw1h, n64(cr)
	return r
}

// A record is priced by the newest price for its model in effect at its
// time; a model with no price then is unpriced, never guessed at.
func TestIssueUsageCost(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now() // 12:00
	is := mustCreate(t, s, NewIssue{Title: "work"})
	mustSetPrice(t, s, "example-large", day("2026-01-01"), rates(3_000_000, 15_000_000, 3_750_000, 6_000_000, 300_000))
	mustSetPrice(t, s, "example-large", t0.Add(5*time.Minute), rates(1_000_000, 2_000_000, 1_250_000, 2_000_000, 100_000))
	mustSetPrice(t, s, "late", t0.Add(30*time.Minute), rates(1, 1, 1, 1, 1))

	clk.add(time.Minute)
	if _, _, err := s.StartIssue(ctx, alice, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * time.Minute)
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{}); err != nil {
		t.Fatal(err)
	}
	mustAddUsage(t, s, alice,
		// Old price: 100×3e6 + 10×15e6 + (40-10)×3.75e6 + 10×6e6 + 1000×3e5.
		cacheRec("r1", "example-large", t0.Add(2*time.Minute), 100, 10, 40, n64(10), 1000),
		// The new price takes effect at its time exactly: 1×1e6.
		rec("r2", "example-large", t0.Add(5*time.Minute), 1, 0),
		// New price; an unknown one-hour part is a five-minute write:
		// 50×1e6 + 5×2e6 + 20×1.25e6.
		cacheRec("r3", "example-large", t0.Add(6*time.Minute), 50, 5, 20, nil, 0),
		rec("r4", "unknown-model", t0.Add(7*time.Minute), 7, 0),
		rec("r5", "late", t0.Add(8*time.Minute), 5, 0), // priced only from later
	)
	u := mustUsage(t, s, is.ID)
	if got, want := picos(u.Cost), "1008500000"; got != want || !u.Cost.Unpriced {
		t.Errorf("IssueUsage(%s).Cost = %s picodollars, unpriced %v; want %s, unpriced", is.ID, got, u.Cost.Unpriced, want)
	}

	// The same tokens with every model priced are fully priced.
	other := mustCreate(t, s, NewIssue{Title: "priced"})
	clk.add(time.Minute)
	if _, _, err := s.StartIssue(ctx, bob, other.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	mustAddUsage(t, s, bob, rec("b1", "example-large", clk.now(), 2, 1))
	clk.add(time.Minute)
	u = mustUsage(t, s, other.ID)
	if got, want := picos(u.Cost), "4000000"; got != want || u.Cost.Unpriced {
		t.Errorf("IssueUsage(%s).Cost = %s picodollars, unpriced %v; want %s, priced", other.ID, got, u.Cost.Unpriced, want)
	}
}

// A record shared by two issues is priced part by part, and the parts'
// costs add up to the whole record's, to the picodollar.
func TestIssueUsageCostOfSplitRecord(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	mustSetPrice(t, s, "example-large", day("2026-01-01"), rates(3_000_001, 15_000_007, 3_750_011, 6_000_013, 300_017))
	a, b := mustCreate(t, s, NewIssue{Title: "a"}), mustCreate(t, s, NewIssue{Title: "b"})
	for _, id := range []IssueID{a.ID, b.ID} {
		if _, _, err := s.StartIssue(ctx, alice, id, time.Hour, false); err != nil {
			t.Fatal(err)
		}
	}
	clk.add(time.Minute)
	r := cacheRec("shared", "example-large", clk.now(), 1001, 333, 77, n64(31), 9999)
	mustAddUsage(t, s, alice, r)
	clk.add(time.Minute)

	whole := new(big.Int)
	for _, c := range []struct{ n, rate int64 }{
		{1001, 3_000_001}, {333, 15_000_007}, {77 - 31, 3_750_011}, {31, 6_000_013}, {9999, 300_017},
	} {
		whole.Add(whole, new(big.Int).Mul(big.NewInt(c.n), big.NewInt(c.rate)))
	}
	ua, ub := mustUsage(t, s, a.ID), mustUsage(t, s, b.ID)
	if !ua.Split || !ub.Split {
		t.Errorf("split = %v and %v, want both split", ua.Split, ub.Split)
	}
	sum := new(big.Int)
	for _, c := range []Cost{ua.Cost, ub.Cost} {
		if c.Picodollars == nil || c.Picodollars.Sign() <= 0 {
			t.Fatalf("a part's cost = %s, want a positive part", picos(c))
		}
		sum.Add(sum, c.Picodollars)
	}
	if sum.Cmp(whole) != 0 {
		t.Errorf("parts cost %s + %s = %s picodollars, want the whole record's %s", picos(ua.Cost), picos(ub.Cost), sum, whole)
	}
}

// A digest prices the window's tokens as it counts them: all of them, or
// a label's issues' parts.
func TestDigestUsageCost(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	mustSetPrice(t, s, "example-large", day("2026-01-01"), rates(2_000_000, 0, 0, 0, 0))
	x := mustCreate(t, s, NewIssue{Title: "labeled", Labels: []string{"web"}})
	if _, _, err := s.StartIssue(ctx, alice, x.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * time.Minute)
	if _, _, err := s.FinishIssue(ctx, alice, x.ID, 0, Finish{}); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * time.Minute)
	mustAddUsage(t, s, alice,
		rec("x1", "example-large", t0.Add(5*time.Minute), 100, 0),
		rec("loose", "example-large", t0.Add(15*time.Minute), 7, 0),
		rec("odd", "unknown-model", t0.Add(16*time.Minute), 1, 0))
	tests := []struct {
		name     string
		f        DigestFilter
		picos    string
		unpriced bool
	}{
		{"everything", DigestFilter{Window: time.Hour}, "214000000", true},
		{"label web", DigestFilter{Window: time.Hour, Label: "web"}, "200000000", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := s.Digest(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if c := d.Usage.Cost; picos(c) != tc.picos || c.Unpriced != tc.unpriced {
				t.Errorf("Digest(%+v).Usage.Cost = %s picodollars, unpriced %v; want %s, %v", tc.f, picos(c), c.Unpriced, tc.picos, tc.unpriced)
			}
		})
	}
}

// groupsText renders a report's groups for comparison:
// "key title in/out $picos [split] [unpriced]".
func groupsText(gs []CostGroup) string {
	var out []string
	for _, g := range gs {
		f := func(p *int64) string {
			if p == nil {
				return "?"
			}
			return fmt.Sprint(*p)
		}
		s := fmt.Sprintf("%s %s/%s $%s", g.Key, f(g.Input), f(g.Output), picos(g.Cost))
		if g.Title != "" {
			s = g.Key + " " + g.Title + s[len(g.Key):]
		}
		if g.Split {
			s += " split"
		}
		if g.Cost.Unpriced {
			s += " unpriced"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

// costFixture records, at 12:00 on the test clock, an epic E (account
// acme) with a task T1, a task T2 (account beta) and a task T3 (no
// account). alice holds T1 12:00-12:10 and T2 12:05-12:10, bob holds T3
// 12:00-12:10. example-large costs 1 USD per million in and 2 out; mystery has no
// price. It returns the issues' ids by name.
func costFixture(t *testing.T) (*Store, map[string]IssueID) {
	t.Helper()
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	mustSetPrice(t, s, "example-large", day("2026-01-01"), rates(1_000_000, 2_000_000, 0, 0, 0))
	e := mustCreate(t, s, NewIssue{Title: "the epic", Type: TypeEpic, Account: "acme"})
	t1 := mustCreate(t, s, NewIssue{Title: "one", ParentID: e.ID})
	t2 := mustCreate(t, s, NewIssue{Title: "two", Account: "beta"})
	t3 := mustCreate(t, s, NewIssue{Title: "three"})
	start := func(a Actor, id IssueID) {
		t.Helper()
		if _, _, err := s.StartIssue(ctx, a, id, time.Hour, false); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(a Actor, id IssueID) {
		t.Helper()
		if _, _, err := s.FinishIssue(ctx, a, id, 0, Finish{}); err != nil {
			t.Fatal(err)
		}
	}
	start(alice, t1.ID)
	start(bob, t3.ID)
	clk.add(5 * time.Minute)
	start(alice, t2.ID)
	clk.add(5 * time.Minute)
	finish(alice, t1.ID)
	finish(alice, t2.ID)
	finish(bob, t3.ID)
	clk.add(50 * time.Minute) // 13:00
	mustAddUsage(t, s, alice,
		rec("a0", "example-large", t0.Add(-30*time.Minute), 999, 0), // before the window
		rec("a1", "example-large", t0.Add(2*time.Minute), 100, 10),  // T1
		rec("a2", "example-large", t0.Add(7*time.Minute), 12, 0),    // T1 and T2, 6 each
		rec("a3", "example-large", t0.Add(20*time.Minute), 1000, 0), // nothing held
		rec("a4", "example-large", t0.Add(time.Hour), 5, 0),         // at the window's end: out
	)
	mustAddUsage(t, s, bob,
		rec("b1", "mystery", t0.Add(3*time.Minute), 50, 0),      // T3, unpriced
		rec("b2", "example-large", t0.Add(4*time.Minute), 7, 0), // T3
	)
	return s, map[string]IssueID{"E": e.ID, "T1": t1.ID, "T2": t2.ID, "T3": t3.ID}
}

func TestCostReport(t *testing.T) {
	s, ids := costFixture(t)
	since := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	tests := []struct {
		by    CostBy
		limit int
		want  string
	}{
		{CostByIssue, 0, "(unattributed) 1000/0 $1000000000; T1 one 106/10 $126000000 split; " +
			"T3 three 57/0 $7000000 unpriced; T2 two 6/0 $6000000 split"},
		{CostByAccount, 0, "(unattributed) 1000/0 $1000000000; acme 106/10 $126000000 split; " +
			"internal 57/0 $7000000 unpriced; beta 6/0 $6000000 split"},
		{CostByEpic, 0, "(unattributed) 1000/0 $1000000000; E the epic 106/10 $126000000 split; " +
			"(no epic) 63/0 $13000000 split unpriced"},
		{CostByPerson, 0, "alice 1112/10 $1132000000; bob 57/0 $7000000 unpriced"},
		{CostByModel, 0, "example-large 1119/10 $1139000000; mystery 50/0 $0 unpriced"},
		// Past the limit, the rest is summed into one group, last.
		{CostByIssue, 2, "(unattributed) 1000/0 $1000000000; T1 one 106/10 $126000000 split; " +
			"(other) 63/0 $13000000 split unpriced"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s limit %d", tc.by, tc.limit), func(t *testing.T) {
			r, err := s.CostReport(t.Context(), CostFilter{By: tc.by, Since: since, Until: until, Account: "internal", Limit: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			for name, id := range ids {
				want = strings.ReplaceAll(want, name+" ", string(id)+" ")
			}
			if got := groupsText(r.Groups); got != want {
				t.Errorf("CostReport(by %s).Groups =\n%s\nwant\n%s", tc.by, got, want)
			}
			if got, want := groupsText([]CostGroup{r.Total}), " 1169/10 $1139000000 unpriced"; got != want {
				t.Errorf("CostReport(by %s).Total = %q, want %q", tc.by, got, want)
			}
			if !slices.Equal(r.Unpriced, []string{"mystery"}) || r.Capped {
				t.Errorf("CostReport(by %s): unpriced %v capped %v; want [mystery], not capped", tc.by, r.Unpriced, r.Capped)
			}
		})
	}
}

// A report reads at most usageScanRows records and says when it left
// some out.
func TestCostReportCapped(t *testing.T) {
	s, _ := costFixture(t)
	setUsageScanRows(t, 2)
	since := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByModel, Since: since, Until: since.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Capped {
		t.Errorf("CostReport with 5 records and a cap of 2: capped = false, want true")
	}
}

func TestCostReportRefuses(t *testing.T) {
	s, clk := clockStore(t)
	now := clk.now()
	tests := []struct {
		name string
		f    CostFilter
		text string
	}{
		{"unknown grouping", CostFilter{By: "team", Since: now.Add(-time.Hour)}, "by"},
		{"no start", CostFilter{By: CostByModel}, "since"},
		{"end before start", CostFilter{By: CostByModel, Since: now, Until: now.Add(-time.Hour)}, "until"},
		{"over a year", CostFilter{By: CostByModel, Since: now.Add(-367 * 24 * time.Hour), Until: now}, "366 days"},
		{"account without a default", CostFilter{By: CostByAccount, Since: now.Add(-time.Hour)}, "account"},
		{"negative limit", CostFilter{By: CostByModel, Since: now.Add(-time.Hour), Limit: -1}, "limit"},
		{"limit too large", CostFilter{By: CostByModel, Since: now.Add(-time.Hour), Limit: MaxCostGroups + 1}, "limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CostReport(t.Context(), tc.f)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("CostReport(%+v) = %v, want ErrInvalid naming %q", tc.f, err, tc.text)
			}
		})
	}
}

// A window back from now reads both ends from one now, so the longest
// window passes on a clock that moves between reads. Taking since from
// one reading and until from a later one made a 366-day window a moment
// longer than 366 days, and always refused it.
func TestCostReportWindow(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: func() time.Time { c.add(time.Millisecond); return c.now() }})
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByModel, Window: MaxCostWindow})
	if err != nil {
		t.Fatalf("CostReport(window %s) = %v, want a report", MaxCostWindow, err)
	}
	if got := r.Until.Sub(r.Since); got != MaxCostWindow {
		t.Errorf("CostReport(window %s) covers %s, want %s", MaxCostWindow, got, MaxCostWindow)
	}
	for _, f := range []CostFilter{
		{By: CostByModel, Window: MaxCostWindow + time.Millisecond},
		{By: CostByModel, Window: -time.Hour},
	} {
		if _, err := s.CostReport(t.Context(), f); !errors.Is(err, ErrInvalid) {
			t.Errorf("CostReport(window %s) = %v, want ErrInvalid", f.Window, err)
		}
	}
}
