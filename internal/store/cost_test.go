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

// summaryCost is a usage summary's cost as text: "none" when the server
// has no prices, else its picodollars, then " unpriced" when some tokens
// had no price.
func summaryCost(c *Cost) string {
	switch {
	case c == nil:
		return "none"
	case c.Unpriced:
		return picos(*c) + " unpriced"
	}
	return picos(*c)
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
	if got, want := summaryCost(u.Cost), "1008500000 unpriced"; got != want {
		t.Errorf("IssueUsage(%s).Cost = %s, want %s", is.ID, got, want)
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
	if got, want := summaryCost(u.Cost), "4000000"; got != want {
		t.Errorf("IssueUsage(%s).Cost = %s, want %s", other.ID, got, want)
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
	for _, c := range []*Cost{ua.Cost, ub.Cost} {
		if c == nil || c.Picodollars == nil || c.Picodollars.Sign() <= 0 {
			t.Fatalf("a part's cost = %s, want a positive part", summaryCost(c))
		}
		sum.Add(sum, c.Picodollars)
	}
	if sum.Cmp(whole) != 0 {
		t.Errorf("parts cost %s + %s = %s picodollars, want the whole record's %s", summaryCost(ua.Cost), summaryCost(ub.Cost), sum, whole)
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
		name string
		f    DigestFilter
		want string
	}{
		{"everything", DigestFilter{Window: time.Hour}, "214000000 unpriced"},
		{"label web", DigestFilter{Window: time.Hour, Label: "web"}, "200000000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := s.Digest(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := summaryCost(d.Usage.Cost); got != tc.want {
				t.Errorf("Digest(%+v).Usage.Cost = %s, want %s", tc.f, got, tc.want)
			}
		})
	}
}

// A server with no prices at all has no cost to give, so a summary
// carries none rather than calling every token unpriced. Once any price
// exists, a model without one is unpriced.
func TestUsageCostWithoutPrices(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	mustAddUsage(t, s, alice, rec("r1", "example-large", clk.now(), 10, 1))
	clk.add(time.Minute)
	check := func(want string) {
		t.Helper()
		if got := summaryCost(mustUsage(t, s, is.ID).Cost); got != want {
			t.Errorf("IssueUsage(%s).Cost = %s, want %s", is.ID, got, want)
		}
		d, err := s.Digest(ctx, DigestFilter{Window: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if got := summaryCost(d.Usage.Cost); got != want {
			t.Errorf("Digest.Usage.Cost = %s, want %s", got, want)
		}
	}
	check("none")
	mustSetPrice(t, s, "example-small", day("2026-01-01"), rates(1, 1, 1, 1, 1))
	check("0 unpriced")
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
		if g.Logged > 0 {
			s += " logged " + g.Logged.String()
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
			// However grouped, the groups add up to the total exactly.
			var in, out int64
			pico := new(big.Int)
			for _, g := range r.Groups {
				in, out = in+*g.Input, out+*g.Output
				if g.Cost.Picodollars != nil {
					pico.Add(pico, g.Cost.Picodollars)
				}
			}
			if in != *r.Total.Input || out != *r.Total.Output || pico.Cmp(r.Total.Cost.Picodollars) != 0 {
				t.Errorf("CostReport(by %s) groups sum to %d/%d $%s, want the total %s", tc.by, in, out, pico, groupsText([]CostGroup{r.Total}))
			}
		})
	}

	// An issue's group in a report costs what show gives it, when the
	// window holds all its records.
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByIssue, Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"T1", "T2", "T3"} {
		id := ids[name]
		i := slices.IndexFunc(r.Groups, func(g CostGroup) bool { return g.Key == string(id) })
		if i < 0 {
			t.Errorf("CostReport(by issue) has no group %s (%s)", id, name)
			continue
		}
		u := mustUsage(t, s, id)
		g := r.Groups[i]
		if got, want := summaryCost(u.Cost), summaryCost(&g.Cost); got != want || len(u.Models) == 0 {
			t.Errorf("IssueUsage(%s).Cost = %s, want the report's %s", id, got, want)
		}
		var uin int64
		for _, m := range u.Models {
			uin += *m.Input
		}
		if uin != *g.Input {
			t.Errorf("IssueUsage(%s) has %d input tokens, want the report's %d", id, uin, *g.Input)
		}
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

// A turn record spanning two holds and time on either side is cut at the
// holds' edges: each issue gets the tokens of its time, and the time no
// issue was held is unattributed. Each issue's part is priced as its own.
func TestCostReportSpanCutAtHolds(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now() // 12:00
	mustSetPrice(t, s, "example-large", day("2026-01-01"), rates(1_000_000, 0, 0, 0, 0))
	a, b := mustCreate(t, s, NewIssue{Title: "a"}), mustCreate(t, s, NewIssue{Title: "b"})
	for _, id := range []IssueID{a.ID, b.ID} { // a 12:00-12:02, then b 12:02-12:04
		if _, _, err := s.StartIssue(ctx, alice, id, time.Hour, false); err != nil {
			t.Fatal(err)
		}
		clk.add(2 * time.Minute)
		if _, _, err := s.FinishIssue(ctx, alice, id, 0, Finish{}); err != nil {
			t.Fatal(err)
		}
	}
	clk.add(10 * time.Minute)
	// 11:58 to 12:06: two minutes each held, four not.
	mustAddUsage(t, s, alice, UsageRecord{Harness: "codex", RequestID: "turn", Model: "example-large", At: t0.Add(6 * time.Minute),
		Granularity: GranularityTurn, SpanStart: ptr(t0.Add(-2 * time.Minute)), Tokens: Tokens{Input: n64(800), Output: n64(0)}})
	r, err := s.CostReport(ctx, CostFilter{By: CostByIssue, Since: t0.Add(-time.Hour), Until: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	title := map[IssueID]string{a.ID: "a", b.ID: "b"}
	first, second := min(a.ID, b.ID), max(a.ID, b.ID) // equal groups go by key
	want := fmt.Sprintf("(unattributed) 400/0 $400000000 split; %s %s 200/0 $200000000 split; %s %s 200/0 $200000000 split",
		first, title[first], second, title[second])
	if got := groupsText(r.Groups); got != want {
		t.Errorf("CostReport(by issue).Groups =\n%s\nwant\n%s", got, want)
	}
}

// An issue's account and epic come from its nearest ancestor that sets
// one, however many generations up; a parent that no longer exists ends
// the chain, so the issue takes the default account and no epic.
func TestCostReportChains(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	mustSetPrice(t, s, "example-large", day("2026-01-01"), rates(1_000_000, 0, 0, 0, 0))
	e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic, Account: "acme"})
	f := mustCreate(t, s, NewIssue{Title: "feature", ParentID: e.ID})
	g := mustCreate(t, s, NewIssue{Title: "grandchild", ParentID: f.ID})
	h := mustCreate(t, s, NewIssue{Title: "orphan", ParentID: f.ID})
	// No foreign key holds parent_id, so a parent can go missing.
	if _, err := s.w.ExecContext(ctx, `UPDATE issues SET parent_id = 'tst-gone', write_id = ? WHERE id = ?`, randomInt63(), h.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		a  Actor
		id IssueID
	}{{alice, g.ID}, {bob, h.ID}} {
		if _, _, err := s.StartIssue(ctx, c.a, c.id, time.Hour, false); err != nil {
			t.Fatal(err)
		}
	}
	clk.add(time.Minute)
	mustAddUsage(t, s, alice, rec("g1", "example-large", clk.now(), 30, 0))
	mustAddUsage(t, s, bob, rec("h1", "example-large", clk.now(), 20, 0))
	clk.add(time.Minute)
	tests := []struct {
		by   CostBy
		want string
	}{
		{CostByAccount, "acme 30/0 $30000000; internal 20/0 $20000000"},
		{CostByEpic, fmt.Sprintf("%s epic 30/0 $30000000; (no epic) 20/0 $20000000", e.ID)},
	}
	for _, tc := range tests {
		r, err := s.CostReport(ctx, CostFilter{By: tc.by, Since: t0, Until: clk.now(), Account: "internal"})
		if err != nil {
			t.Fatal(err)
		}
		if got := groupsText(r.Groups); got != tc.want {
			t.Errorf("CostReport(by %s).Groups = %q, want %q", tc.by, got, tc.want)
		}
	}
}

// Hours logged on the days a report's window overlaps join the groups:
// an issue's by its issue, account or epic, a person's by who logged
// them, and all of them as (human) by model, so under every grouping the
// groups' hours add up to the total. An issue with hours and no tokens
// is a group of its own.
func TestCostReportHours(t *testing.T) {
	s, ids := costFixture(t) // now 13:00 on 2026-10-07
	t4 := mustCreate(t, s, NewIssue{Title: "four"})
	ids["T4"] = t4.ID
	mustLogHours(t, s, alice, NewHours{Issue: ids["T1"], Duration: time.Hour})
	mustLogHours(t, s, bob, NewHours{Issue: ids["T3"], Duration: 2 * time.Hour})
	mustLogHours(t, s, alice, NewHours{Issue: t4.ID, Duration: 15 * time.Minute})
	mustLogHours(t, s, alice, NewHours{Issue: ids["T2"], Duration: 8 * time.Hour, On: day("2026-10-06")}) // outside
	since := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	tests := []struct {
		by   CostBy
		want string
	}{
		{CostByIssue, "(unattributed) 1000/0 $1000000000; T1 one 106/10 $126000000 split logged 1h0m0s; " +
			"T3 three 57/0 $7000000 unpriced logged 2h0m0s; T2 two 6/0 $6000000 split; T4 four ?/? $0 logged 15m0s"},
		{CostByAccount, "(unattributed) 1000/0 $1000000000; acme 106/10 $126000000 split logged 1h0m0s; " +
			"internal 57/0 $7000000 unpriced logged 2h15m0s; beta 6/0 $6000000 split"},
		{CostByEpic, "(unattributed) 1000/0 $1000000000; E the epic 106/10 $126000000 split logged 1h0m0s; " +
			"(no epic) 63/0 $13000000 split unpriced logged 2h15m0s"},
		{CostByPerson, "alice 1112/10 $1132000000 logged 1h15m0s; bob 57/0 $7000000 unpriced logged 2h0m0s"},
		{CostByModel, "example-large 1119/10 $1139000000; mystery 50/0 $0 unpriced; (human) ?/? $0 logged 3h15m0s"},
	}
	for _, tc := range tests {
		t.Run(string(tc.by), func(t *testing.T) {
			r, err := s.CostReport(t.Context(), CostFilter{By: tc.by, Since: since, Until: until, Account: "internal"})
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
			if r.Total.Logged != 3*time.Hour+15*time.Minute {
				t.Errorf("CostReport(by %s).Total.Logged = %s, want 3h15m0s", tc.by, r.Total.Logged)
			}
		})
	}
	// Past the limit, the rest's hours are summed with the rest.
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByIssue, Since: since, Until: until, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if last := r.Groups[len(r.Groups)-1]; last.Key != "(other)" || last.Logged != 2*time.Hour+15*time.Minute {
		t.Errorf("CostReport(by issue, limit 2) last group %s logged %s, want (other) logged 2h15m0s", last.Key, last.Logged)
	}
}
