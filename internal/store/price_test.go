package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rates are invented list rates, in micro-dollars per million tokens.
func rates(in, out, cw, cw1h, cr int64) Rates {
	return Rates{Input: in, Output: out, CacheWrite: cw, CacheWrite1h: cw1h, CacheRead: cr}
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err) // a test's constant
	}
	return t
}

func mustSetPrice(t *testing.T, s *Store, model string, from time.Time, r Rates) PriceChange {
	t.Helper()
	c, err := s.SetPrice(t.Context(), dana, model, from, r)
	if err != nil {
		t.Fatalf("SetPrice(%s, %s): %v", model, from.Format(time.DateOnly), err)
	}
	return c
}

// pricesText renders prices for comparison: "model@date in/out/cw/cw1h/cr by who".
func pricesText(ps []Price) string {
	var out []string
	for _, p := range ps {
		r := p.Rates
		out = append(out, fmt.Sprintf("%s@%s %d/%d/%d/%d/%d by %s", p.Model, p.From.Format(time.DateOnly),
			r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead, p.SetBy))
	}
	return strings.Join(out, "; ")
}

func TestSetPriceAddsReplacesAndLists(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	before := lastSeq(t, s)
	steps := []struct {
		name  string
		model string
		from  string
		r     Rates
		want  PriceChange
	}{
		{"first price", "model-b", "2026-01-01", rates(1, 2, 3, 4, 5), PriceAdded},
		{"a later price", "model-b", "2026-06-01", rates(10, 20, 30, 40, 50), PriceAdded},
		{"another model", "model-a", "2026-03-01", rates(7, 7, 7, 7, 7), PriceAdded},
		{"same key, new rates", "model-b", "2026-06-01", rates(11, 21, 31, 41, 51), PriceReplaced},
		{"same key, same rates", "model-b", "2026-06-01", rates(11, 21, 31, 41, 51), PriceUnchanged},
		{"case is another model", "Model-A", "2026-03-01", rates(8, 8, 8, 8, 8), PriceAdded},
	}
	for _, st := range steps {
		clk.add(time.Minute)
		if got := mustSetPrice(t, s, st.model, day(st.from), st.r); got != st.want {
			t.Errorf("%s: SetPrice = %q, want %q", st.name, got, st.want)
		}
	}
	ps, err := s.Prices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "Model-A@2026-03-01 8/8/8/8/8 by dana; model-a@2026-03-01 7/7/7/7/7 by dana; " +
		"model-b@2026-01-01 1/2/3/4/5 by dana; model-b@2026-06-01 11/21/31/41/51 by dana"
	if got := pricesText(ps); got != want {
		t.Errorf("Prices() =\n%s\nwant\n%s", got, want)
	}

	// One price.set event per change, none for the unchanged set; a
	// replace records the rates it replaced.
	evs, err := s.Events(ctx, before, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sets []Event
	for _, e := range evs {
		if e.Op == OpPriceSet {
			sets = append(sets, e)
		}
	}
	if len(sets) != 5 {
		t.Fatalf("price.set events = %d, want 5 (one per change)", len(sets))
	}
	replace := sets[3]
	if replace.Target != PriceTarget || replace.Actor != dana {
		t.Errorf("replace event target %q actor %+v, want %q and dana", replace.Target, replace.Actor, PriceTarget)
	}
	if b, a := string(replace.Before), string(replace.After); !strings.Contains(b, `"input":10`) || !strings.Contains(a, `"input":11`) ||
		!strings.Contains(a, `"model":"model-b"`) {
		t.Errorf("replace event before %s after %s, want the old and new rates of model-b", b, a)
	}
	if sets[0].Before != nil {
		t.Errorf("add event before = %s, want none", sets[0].Before)
	}
}

func TestSetPriceRefuses(t *testing.T) {
	s, clk := clockStore(t)
	now := clk.now()
	ok := rates(1, 1, 1, 1, 1)
	tests := []struct {
		name  string
		actor Actor
		model string
		from  time.Time
		r     Rates
		want  error
		text  string
	}{
		{"not an admin", alice, "model-a", day("2026-01-01"), ok, ErrForbidden, "prices set is for admins"},
		{"bad model", dana, "-model", day("2026-01-01"), ok, ErrInvalid, "model"},
		{"model with a space", dana, "model a", day("2026-01-01"), ok, ErrInvalid, "model"},
		{"before 2020", dana, "model-a", day("2019-12-31"), ok, ErrInvalid, "from"},
		{"over a year ahead", dana, "model-a", now.Add(367 * 24 * time.Hour), ok, ErrInvalid, "from"},
		{"negative rate", dana, "model-a", day("2026-01-01"), rates(1, -1, 1, 1, 1), ErrInvalid, "output"},
		{"rate too large", dana, "model-a", day("2026-01-01"), rates(1, 1, 1, MaxRate+1, 1), ErrInvalid, "cache_write_1h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SetPrice(t.Context(), tc.actor, tc.model, tc.from, tc.r)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("SetPrice(%s, %q) = %v, want %v naming %q", tc.actor.Principal, tc.model, err, tc.want, tc.text)
			}
		})
	}
	ps, err := s.Prices(t.Context())
	if err != nil || len(ps) != 0 {
		t.Errorf("Prices() after refusals = %v, %v; want none", ps, err)
	}
	if _, err := s.SetPrice(t.Context(), dana, "model-a", now.Add(366*24*time.Hour), ok); err != nil {
		t.Errorf("SetPrice a year ahead: %v, want nil", err)
	}
}

// Rule 18: prices are bounded by limits: prices; replacing a price at the
// cap is no new row.
func TestSetPriceLimit(t *testing.T) {
	s := openStore(t, newDSN(t), Options{Limits: Limits{Prices: 2}})
	mustSetPrice(t, s, "model-a", day("2026-01-01"), rates(1, 1, 1, 1, 1))
	mustSetPrice(t, s, "model-a", day("2026-02-01"), rates(1, 1, 1, 1, 1))
	_, err := s.SetPrice(t.Context(), dana, "model-b", day("2026-01-01"), rates(1, 1, 1, 1, 1))
	le, ok := errors.AsType[*PriceLimitError](err)
	if !ok || le.Max != 2 || !errors.Is(err, ErrInvalid) {
		t.Fatalf("third price = %v, want a *PriceLimitError of 2 wrapping ErrInvalid", err)
	}
	if got := mustSetPrice(t, s, "model-a", day("2026-02-01"), rates(2, 2, 2, 2, 2)); got != PriceReplaced {
		t.Errorf("replace at the cap = %q, want %q", got, PriceReplaced)
	}
}
