package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// fixedNow is the clock of the hours and plans tests: noon on 7 Oct
// 2026, so a day and a month never turn under them.
func fixedNow() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }

// A person logs, lists and undoes their hours; show, digest and cost
// carry them.
func TestDispatchHours(t *testing.T) {
	s, _ := newServerClock(t, Limits{}, fixedNow)
	is := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mine := mustCall[proto.HoursLogResult](t, s, alice, proto.OpHoursLog, proto.HoursLogArgs{ID: is.ID, Seconds: 5400, Note: "pairing", Idem: "cli-1"})
	if mine.ID == "" || mine.On != "2026-10-07" {
		t.Errorf("hours.log(today) = %+v, want an entry on 2026-10-07", mine)
	}
	if again := mustCall[proto.HoursLogResult](t, s, alice, proto.OpHoursLog, proto.HoursLogArgs{ID: is.ID, Seconds: 5400, Note: "pairing", Idem: "cli-1"}); again != mine {
		t.Errorf("hours.log retried = %+v, want the first entry %+v", again, mine)
	}
	mustCall[proto.HoursLogResult](t, s, bob, proto.OpHoursLog, proto.HoursLogArgs{ID: is.ID, Seconds: 7200, On: "2026-10-06"})

	list := mustCall[proto.HoursResult](t, s, bob, proto.OpHours, proto.HoursArgs{Issue: is.ID})
	if len(list.Entries) != 2 || list.Entries[0] != (proto.HoursEntry{ID: mine.ID, Issue: is.ID, Principal: "alice", On: "2026-10-07",
		Seconds: 5400, Note: "pairing", At: fixedNow()}) || list.Entries[1].Principal != "bob" || list.More != 0 {
		t.Errorf("hours(issue) = %+v, want alice's 5400 s today, then bob's", list)
	}
	if l := mustCall[proto.HoursResult](t, s, bob, proto.OpHours, proto.HoursArgs{By: "bob", Limit: 1}); len(l.Entries) != 1 || l.Entries[0].Principal != "bob" {
		t.Errorf("hours(by bob) = %+v, want bob's entry", l)
	}

	u := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID}).Usage
	if u == nil || u.LoggedSeconds != 12600 || len(u.Logged) != 2 || u.Logged[0] != (proto.PersonHours{Principal: "bob", Seconds: 7200}) {
		t.Errorf("show(%s).usage = %+v, want 12600 s logged, bob's 7200 first", is.ID, u)
	}
	if d := mustCall[proto.DigestResult](t, s, alice, proto.OpDigest, proto.DigestArgs{Since: "24h"}).Usage; d == nil || d.LoggedSeconds != 12600 {
		t.Errorf("digest(24h).usage = %+v, want 12600 s logged", d)
	}
	r := mustCall[proto.CostResult](t, s, alice, proto.OpCost, proto.CostArgs{By: "person", Since: "2026-10-01"})
	if len(r.Groups) != 2 || r.Groups[0].Key != "bob" || r.Groups[0].LoggedSeconds != 7200 || r.Total.LoggedSeconds != 12600 ||
		r.Groups[0].AmortizedUSD != "" {
		t.Errorf("cost(by person) = %+v, want bob's 7200 s first, 12600 s in all, no amortized cost", r)
	}

	tests := []struct {
		name  string
		actor store.Actor
		op    string
		args  string
		code  proto.Code
		msg   string
		fix   string
	}{
		{"under a minute", alice, proto.OpHoursLog, `{"id":"` + is.ID + `","seconds":30}`, proto.CodeInvalid, "duration", "sfx log -h"},
		{"over a day", alice, proto.OpHoursLog, `{"id":"` + is.ID + `","seconds":9223372036854775807}`, proto.CodeInvalid, "seconds", "sfx log -h"},
		{"bad day", alice, proto.OpHoursLog, `{"id":"` + is.ID + `","seconds":60,"on":"tomorrow"}`, proto.CodeInvalid, `on "tomorrow"`, "sfx log -h"},
		{"no issue", alice, proto.OpHoursLog, `{"id":"sf-none","seconds":60}`, proto.CodeNotFound, "sf-none", "sfx list"},
		{"unknown field", alice, proto.OpHoursLog, `{"id":"` + is.ID + `","seconds":60,"rate":300}`, proto.CodeInvalid, "unknown field", proto.FixUpgrade},
		{"another's entry", bob, proto.OpHoursDelete, `{"id":"` + mine.ID + `"}`, proto.CodeForbidden,
			"undoing another person's hours is for starfix admins", "ask a starfix admin"},
		{"no entry", alice, proto.OpHoursDelete, `{"id":"nosuchentry"}`, proto.CodeNotFound, "hours entry nosuchentry not found", "sfx log --issue"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := s.Dispatch(t.Context(), tc.actor, tc.op, json.RawMessage(tc.args))
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Fatalf("%s(%s) = %+v; want %s naming %q, fix with %q", tc.op, tc.args, perr, tc.code, tc.msg, tc.fix)
			}
		})
	}
	if got := mustCall[proto.HoursDeleteResult](t, s, alice, proto.OpHoursDelete, proto.HoursDeleteArgs{ID: mine.ID}); got.ID != mine.ID {
		t.Errorf("hours.delete(own entry) = %+v, want %s", got, mine.ID)
	}
}

// Rule 18: past limits: hours_per_day a new entry is refused, naming the
// limit and how to combine entries.
func TestDispatchHoursLimit(t *testing.T) {
	s, _ := newServerClock(t, Limits{Limits: store.Limits{HoursPerDay: 1}}, fixedNow)
	is := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.HoursLogResult](t, s, alice, proto.OpHoursLog, proto.HoursLogArgs{ID: is.ID, Seconds: 60})
	_, perr := call[proto.HoursLogResult](t, s, alice, proto.OpHoursLog, proto.HoursLogArgs{ID: is.ID, Seconds: 60})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "alice has 1 entries on 2026-10-07") ||
		!strings.Contains(perr.Fix, "sfx log --undo") || !strings.Contains(perr.Fix, "hours_per_day under limits:") {
		t.Fatalf("a second entry with hours_per_day 1: %+v, want invalid naming the limit", perr)
	}
}

func TestDispatchPlans(t *testing.T) {
	s, _ := newServerClock(t, Limits{}, fixedNow)
	set := proto.PlanSetArgs{Name: "team", From: "2026-09", Fee: 10_000_000, Seats: 3, Principals: []string{"bob", "alice"}}
	for _, want := range []string{"added", "unchanged"} {
		if got := mustCall[proto.PlanSetResult](t, s, dana, proto.OpPlanSet, set); got.Change != want {
			t.Errorf("plan.set by an admin = %q, want %q", got.Change, want)
		}
	}
	got := mustCall[proto.PlansResult](t, s, alice, proto.OpPlans, proto.PlansArgs{})
	if len(got.Plans) != 1 || got.Plans[0].Name != "team" || got.Plans[0].From != "2026-09" || got.Plans[0].Fee != 10_000_000 ||
		got.Plans[0].Seats != 3 || strings.Join(got.Plans[0].Principals, ",") != "alice,bob" || got.Plans[0].SetBy != "dana" {
		t.Errorf("plans = %+v, want team from 2026-09, 10 USD × 3, alice and bob, set by dana", got.Plans)
	}
	tests := []struct {
		name  string
		actor store.Actor
		args  string
		code  proto.Code
		msg   string
		fix   string
	}{
		{"not an admin", alice, `{"name":"team","from":"2026-09","fee":1,"seats":1}`, proto.CodeForbidden, "plans set is for starfix admins", "ask a starfix admin"},
		{"bad from", dana, `{"name":"team","from":"2026-13","fee":1,"seats":1}`, proto.CodeInvalid, `from "2026-13"`, "sfx admin plans set -h"},
		{"a date", dana, `{"name":"team","from":"2026-09-01","fee":1,"seats":1}`, proto.CodeInvalid, `from "2026-09-01"`, "sfx admin plans set -h"},
		{"bad name", dana, `{"name":"Team","from":"2026-09","fee":1,"seats":1}`, proto.CodeInvalid, "name", "sfx admin plans set -h"},
		{"unknown field", dana, `{"name":"team","from":"2026-09","fee":1,"seats":1,"currency":"EUR"}`, proto.CodeInvalid, "unknown field", proto.FixUpgrade},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := s.Dispatch(t.Context(), tc.actor, proto.OpPlanSet, json.RawMessage(tc.args))
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Fatalf("plan.set(%s) = %+v; want %s naming %q, fix with %q", tc.args, perr, tc.code, tc.msg, tc.fix)
			}
		})
	}
}

// Rule 18: past limits: plans a new plan row is refused, naming the limit.
func TestDispatchPlansLimit(t *testing.T) {
	s, _ := newServerClock(t, Limits{Limits: store.Limits{Plans: 1}}, fixedNow)
	mustCall[proto.PlanSetResult](t, s, dana, proto.OpPlanSet, proto.PlanSetArgs{Name: "a", From: "2026-09", Fee: 1, Seats: 1})
	_, perr := call[proto.PlanSetResult](t, s, dana, proto.OpPlanSet, proto.PlanSetArgs{Name: "b", From: "2026-09", Fee: 1, Seats: 1})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "at most 1 plan rows") ||
		!strings.Contains(perr.Fix, "plans under limits:") {
		t.Fatalf("a second plan with plans 1: %+v, want invalid naming the limit", perr)
	}
}

// A cost report carries every group's amortized cost, an exact decimal,
// once the server has a plan.
func TestDispatchCostAmortized(t *testing.T) {
	var mu sync.Mutex
	now := fixedNow().Add(-time.Hour)
	s, _ := newServerClock(t, Limits{}, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	is := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: is.ID})
	mu.Lock()
	now = fixedNow()
	mu.Unlock()
	at := fixedNow().Add(-55 * time.Minute) // while alice holds the issue, under its first lease
	mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, proto.UsageArgs{Records: []proto.UsageRecord{usageRec("r1", at, 300)}})
	mustCall[proto.UsageResult](t, s, bob, proto.OpUsage, proto.UsageArgs{Records: []proto.UsageRecord{usageRec("r2", at, 100)}})
	mustCall[proto.PlanSetResult](t, s, dana, proto.OpPlanSet, proto.PlanSetArgs{Name: "team", From: "2026-10", Fee: 10_000_001, Seats: 1,
		Principals: []string{"alice"}})
	r := mustCall[proto.CostResult](t, s, bob, proto.OpCost, proto.CostArgs{By: "issue", Since: "2026-10-01", Until: "2026-11-01"})
	want := map[string]string{is.ID: "10.000001", proto.CostUnattributed: "0"}
	if len(r.Groups) != 2 || r.Total.AmortizedUSD != "10.000001" {
		t.Fatalf("cost(by issue) = %+v, want two groups and 10.000001 USD amortized in all", r)
	}
	for _, g := range r.Groups {
		if g.AmortizedUSD != want[g.Key] {
			t.Errorf("cost(by issue) group %s amortized %q, want %q", g.Key, g.AmortizedUSD, want[g.Key])
		}
	}
}

func TestHoursAndPlansReads(t *testing.T) {
	for _, op := range []string{proto.OpHours, proto.OpPlans} {
		if !readOps[op] {
			t.Errorf("%s is not a read op", op)
		}
	}
	for _, op := range []string{proto.OpHoursLog, proto.OpHoursDelete, proto.OpPlanSet} {
		if readOps[op] {
			t.Errorf("%s is a read op", op)
		}
	}
}
