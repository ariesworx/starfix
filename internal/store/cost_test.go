package store

import (
	"math/big"
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
	mustSetPrice(t, s, "opus", day("2026-01-01"), rates(3_000_000, 15_000_000, 3_750_000, 6_000_000, 300_000))
	mustSetPrice(t, s, "opus", t0.Add(5*time.Minute), rates(1_000_000, 2_000_000, 1_250_000, 2_000_000, 100_000))
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
		cacheRec("r1", "opus", t0.Add(2*time.Minute), 100, 10, 40, n64(10), 1000),
		// The new price takes effect at its time exactly: 1×1e6.
		rec("r2", "opus", t0.Add(5*time.Minute), 1, 0),
		// New price; an unknown one-hour part is a five-minute write:
		// 50×1e6 + 5×2e6 + 20×1.25e6.
		cacheRec("r3", "opus", t0.Add(6*time.Minute), 50, 5, 20, nil, 0),
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
	mustAddUsage(t, s, bob, rec("b1", "opus", clk.now(), 2, 1))
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
	mustSetPrice(t, s, "opus", day("2026-01-01"), rates(3_000_001, 15_000_007, 3_750_011, 6_000_013, 300_017))
	a, b := mustCreate(t, s, NewIssue{Title: "a"}), mustCreate(t, s, NewIssue{Title: "b"})
	for _, id := range []IssueID{a.ID, b.ID} {
		if _, _, err := s.StartIssue(ctx, alice, id, time.Hour, false); err != nil {
			t.Fatal(err)
		}
	}
	clk.add(time.Minute)
	r := cacheRec("shared", "opus", clk.now(), 1001, 333, 77, n64(31), 9999)
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
	mustSetPrice(t, s, "claude-opus-4-1", day("2026-01-01"), rates(2_000_000, 0, 0, 0, 0))
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
		req("x1", t0.Add(5*time.Minute), 100, 0),
		req("loose", t0.Add(15*time.Minute), 7, 0),
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
