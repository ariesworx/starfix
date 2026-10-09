package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func month(s string) time.Time {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		panic(err) // a test's constant
	}
	return t
}

func mustSetPlan(t *testing.T, s *Store, p NewPlan) PlanChange {
	t.Helper()
	c, err := s.SetPlan(t.Context(), dana, p)
	if err != nil {
		t.Fatalf("SetPlan(%s, %s): %v", p.Name, p.From.Format("2006-01"), err)
	}
	return c
}

// plansText renders plans for comparison: "name@month fee×seats [principals] by who".
func plansText(ps []Plan) string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s@%s %d×%d %v by %s", p.Name, p.From.Format("2006-01"), p.Fee, p.Seats, p.Principals, p.SetBy))
	}
	return strings.Join(out, "; ")
}

func TestSetPlanAddsReplacesAndLists(t *testing.T) {
	s, _ := clockStore(t) // 2026-10-07
	ctx := t.Context()
	before := lastSeq(t, s)
	steps := []struct {
		name string
		p    NewPlan
		want PlanChange
	}{
		{"first plan", NewPlan{Name: "team", From: month("2026-01"), Fee: 30_000_000, Seats: 2, Principals: []string{"bob", "alice"}}, PlanAdded},
		{"a later version", NewPlan{Name: "team", From: month("2026-06"), Fee: 30_000_000, Seats: 3, Principals: []string{"alice"}}, PlanAdded},
		{"another plan", NewPlan{Name: "max", From: month("2026-03"), Fee: 200_000_000, Seats: 1, Principals: []string{"carol"}}, PlanAdded},
		{"same key, new terms", NewPlan{Name: "team", From: month("2026-06"), Fee: 25_000_000, Seats: 3, Principals: []string{"alice", "bob"}}, PlanReplaced},
		{"same key, same terms", NewPlan{Name: "team", From: month("2026-06"), Fee: 25_000_000, Seats: 3, Principals: []string{"bob", "alice", "bob"}}, PlanUnchanged},
		{"ended", NewPlan{Name: "max", From: month("2026-09")}, PlanAdded},
	}
	for _, st := range steps {
		if got := mustSetPlan(t, s, st.p); got != st.want {
			t.Errorf("%s: SetPlan = %s, want %s", st.name, got, st.want)
		}
	}
	ps, err := s.Plans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "max@2026-03 200000000×1 [carol] by dana; max@2026-09 0×0 [] by dana; " +
		"team@2026-01 30000000×2 [alice bob] by dana; team@2026-06 25000000×3 [alice bob] by dana"
	if got := plansText(ps); got != want {
		t.Errorf("Plans =\n%s\nwant\n%s", got, want)
	}

	evs, err := s.events(ctx, `WHERE seq > ? AND op = ? ORDER BY seq`, before, string(OpPlanSet))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 { // the unchanged set writes nothing
		t.Fatalf("plan.set events = %d, want 5", len(evs))
	}
	replace := evs[3]
	var b, a struct {
		Name       string   `json:"name"`
		From       string   `json:"from"`
		Fee        int64    `json:"fee"`
		Seats      int      `json:"seats"`
		Principals []string `json:"principals"`
	}
	if err := json.Unmarshal(replace.Before, &b); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replace.After, &a); err != nil {
		t.Fatal(err)
	}
	if replace.Target != PlanTarget || b.Fee != 30_000_000 || strings.Join(b.Principals, ",") != "alice" ||
		a.Name != "team" || a.From != "2026-06" || a.Fee != 25_000_000 || strings.Join(a.Principals, ",") != "alice,bob" {
		t.Errorf("replace event: target %s, before %+v, after %+v", replace.Target, b, a)
	}
}

func TestSetPlanRefuses(t *testing.T) {
	s, _ := clockStore(t) // 2026-10-07
	ok := NewPlan{Name: "team", From: month("2026-01"), Fee: 1, Seats: 1, Principals: []string{"alice"}}
	if _, err := s.SetPlan(t.Context(), alice, ok); !errors.Is(err, ErrForbidden) {
		t.Errorf("SetPlan as a non-admin = %v, want ErrForbidden", err)
	}
	tests := []struct {
		name string
		edit func(*NewPlan)
		text string
	}{
		{"name", func(p *NewPlan) { p.Name = "Team" }, "name"},
		{"long name", func(p *NewPlan) { p.Name = strings.Repeat("a", 65) }, "name"},
		{"not a month", func(p *NewPlan) { p.From = month("2026-01").AddDate(0, 0, 1) }, "month"},
		{"before 2020", func(p *NewPlan) { p.From = month("2019-12") }, "2020"},
		{"over a year ahead", func(p *NewPlan) { p.From = month("2027-11") }, "year"},
		{"negative fee", func(p *NewPlan) { p.Fee = -1 }, "fee"},
		{"fee too large", func(p *NewPlan) { p.Fee = MaxPlanFee + 1 }, "fee"},
		{"negative seats", func(p *NewPlan) { p.Seats = -1 }, "seats"},
		{"too many seats", func(p *NewPlan) { p.Seats = MaxPlanSeats + 1 }, "seats"},
		{"bad principal", func(p *NewPlan) { p.Principals = []string{"Alice"} }, "principal"},
		{"too many principals", func(p *NewPlan) { p.Principals = []string{"a", "b", "c", "d"} }, "plan_principals"},
	}
	s = openStore(t, newDSN(t), Options{Limits: Limits{PlanPrincipals: 3}, Now: func() time.Time { return day("2026-10-07") }})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ok
			tc.edit(&p)
			_, err := s.SetPlan(t.Context(), dana, p)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("SetPlan(%+v) = %v, want ErrInvalid naming %q", p, err, tc.text)
			}
		})
	}
	mustSetPlan(t, s, NewPlan{Name: "most", From: month("2027-10"), Fee: MaxPlanFee, Seats: MaxPlanSeats, Principals: []string{"a", "b", "c"}})
}

// The plans table holds at most limits: plans rows; replacing one is no
// new row.
func TestSetPlanLimit(t *testing.T) {
	s := openStore(t, newDSN(t), Options{Limits: Limits{Plans: 2}, Now: func() time.Time { return day("2026-10-07") }})
	mustSetPlan(t, s, NewPlan{Name: "a", From: month("2026-01"), Fee: 1, Seats: 1})
	mustSetPlan(t, s, NewPlan{Name: "b", From: month("2026-01"), Fee: 1, Seats: 1})
	_, err := s.SetPlan(t.Context(), dana, NewPlan{Name: "c", From: month("2026-01"), Fee: 1, Seats: 1})
	var capErr *PlanLimitError
	if !errors.As(err, &capErr) || !errors.Is(err, ErrInvalid) || capErr.Max != 2 {
		t.Errorf("SetPlan past plans 2 = %v, want a *PlanLimitError of 2 wrapping ErrInvalid", err)
	}
	if c := mustSetPlan(t, s, NewPlan{Name: "a", From: month("2026-01"), Fee: 2, Seats: 1}); c != PlanReplaced {
		t.Errorf("replacing a plan at the cap = %s, want replaced", c)
	}
}
