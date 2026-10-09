package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// invented list rates, in micro-dollars per million tokens
var testRates = proto.Rates{Input: 2_000_000, Output: 10_000_000, CacheWrite: 2_500_000, CacheWrite1h: 4_000_000, CacheRead: 200_000}

func TestDispatchPrices(t *testing.T) {
	s := newServer(t)
	set := proto.PriceSetArgs{Model: "example-large", From: "2026-01-01", Rates: testRates}
	for _, want := range []string{"added", "unchanged"} {
		if got := mustCall[proto.PriceSetResult](t, s, dana, proto.OpPriceSet, set); got.Change != want {
			t.Errorf("price.set by an admin = %q, want %q", got.Change, want)
		}
	}
	later := set
	later.From, later.Input = "2026-06-01T09:30:00Z", 1_000_000
	mustCall[proto.PriceSetResult](t, s, dana, proto.OpPriceSet, later)

	got := mustCall[proto.PricesResult](t, s, alice, proto.OpPrices, proto.PricesArgs{})
	if len(got.Prices) != 2 || got.Prices[0].Model != "example-large" || got.Prices[0].From != time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) || got.Prices[0].Rates != testRates ||
		got.Prices[1].From != time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC) || got.Prices[1].Input != 1_000_000 || got.Prices[1].SetBy != "dana" {
		t.Errorf("prices = %+v, want example-large from 2026-01-01 and from 2026-06-01T09:30Z, set by dana", got.Prices)
	}

	tests := []struct {
		name  string
		actor store.Actor
		args  string
		code  proto.Code
		msg   string
		fix   string
	}{
		{"not an admin", alice, `{"model":"example-large","from":"2026-01-01"}`, proto.CodeForbidden,
			"prices set is for starfix admins", "ask a starfix admin"},
		{"bad from", dana, `{"model":"example-large","from":"soon"}`, proto.CodeInvalid, `from "soon"`, "sfx admin prices set -h"},
		{"bad model", dana, `{"model":"example large","from":"2026-01-01"}`, proto.CodeInvalid, "model", "sfx admin prices set -h"},
		{"negative rate", dana, `{"model":"example-large","from":"2026-01-01","output":-1}`, proto.CodeInvalid, "output rate", "sfx admin prices set -h"},
		{"unknown field", dana, `{"model":"example-large","from":"2026-01-01","currency":"EUR"}`, proto.CodeInvalid, "unknown field", proto.FixUpgrade},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := s.Dispatch(t.Context(), tc.actor, proto.OpPriceSet, json.RawMessage(tc.args))
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Fatalf("price.set(%s) = %+v; want %s naming %q, fix with %q", tc.args, perr, tc.code, tc.msg, tc.fix)
			}
		})
	}
}

// Rule 18: past limits: prices a new price is refused, naming the limit.
func TestDispatchPricesLimit(t *testing.T) {
	s := newServerWith(t, Limits{Limits: store.Limits{Prices: 1}})
	mustCall[proto.PriceSetResult](t, s, dana, proto.OpPriceSet, proto.PriceSetArgs{Model: "example-large", From: "2026-01-01", Rates: testRates})
	_, perr := call[proto.PriceSetResult](t, s, dana, proto.OpPriceSet, proto.PriceSetArgs{Model: "example-small", From: "2026-01-01", Rates: testRates})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "at most 1 prices") ||
		!strings.Contains(perr.Fix, "prices under limits:") {
		t.Fatalf("a second price with prices 1: %+v, want invalid naming the limit", perr)
	}
}

func TestPricesAndCostAreReads(t *testing.T) {
	for _, op := range []string{proto.OpPrices, proto.OpCost} {
		if !readOps[op] {
			t.Errorf("%s is not a read op", op)
		}
	}
	if readOps[proto.OpPriceSet] {
		t.Errorf("%s is a read op", proto.OpPriceSet)
	}
}

// show, digest and cost carry the list-price equivalent as an exact
// decimal of US dollars.
func TestDispatchCost(t *testing.T) {
	s := newServer(t)
	mustCall[proto.PriceSetResult](t, s, dana, proto.OpPriceSet, proto.PriceSetArgs{Model: "example-large", From: "2026-01-01", Rates: testRates})
	is := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work", Account: "acme"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: is.ID})
	now := time.Now().UTC()
	since := now.Add(-time.Minute).Format(time.RFC3339Nano)
	rec := usageRec("r1", now, 1_234_567) // 1,234,567 in at 2 USD a million
	rec.Model = "example-large"
	odd := usageRec("r2", now, 10)
	odd.Model = "mystery"
	mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, proto.UsageArgs{Records: []proto.UsageRecord{rec, odd}})

	u := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID}).Usage
	if u == nil || u.CostUSD != "2.469134" || !u.Unpriced {
		t.Errorf("show(%s).usage = %+v, want cost_usd 2.469134, unpriced", is.ID, u)
	}
	d := mustCall[proto.DigestResult](t, s, alice, proto.OpDigest, proto.DigestArgs{Since: "1h"}).Usage
	if d == nil || d.CostUSD != "2.469134" || !d.Unpriced {
		t.Errorf("digest.usage = %+v, want cost_usd 2.469134, unpriced", d)
	}

	r := mustCall[proto.CostResult](t, s, bob, proto.OpCost, proto.CostArgs{By: "account", Since: since})
	if len(r.Groups) != 1 || r.Groups[0].Key != "acme" || r.Groups[0].CostUSD != "2.469134" || !r.Groups[0].Unpriced ||
		r.Total.CostUSD != "2.469134" || r.By != "account" || len(r.Unpriced) != 1 || r.Unpriced[0] != "mystery" {
		t.Errorf("cost by account = %+v, want acme at 2.469134 USD, mystery unpriced", r)
	}
	r = mustCall[proto.CostResult](t, s, bob, proto.OpCost, proto.CostArgs{By: "issue", Since: since})
	if len(r.Groups) != 1 || r.Groups[0].Key != is.ID || r.Groups[0].Title != "work" {
		t.Errorf("cost by issue = %+v, want %s titled work", r.Groups, is.ID)
	}
	// An empty window has no groups and costs nothing.
	r = mustCall[proto.CostResult](t, s, bob, proto.OpCost, proto.CostArgs{By: "model", Since: "2026-01-01", Until: "2026-01-02"})
	if len(r.Groups) != 0 || r.Total.CostUSD != "0" || !r.Until.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("cost of an empty window = %+v, want no groups, 0 USD, until 2026-01-02", r)
	}

	tests := []struct {
		name string
		args string
		msg  string
	}{
		{"unknown grouping", `{"by":"team","since":"7d"}`, `by "team"`},
		{"no since", `{"by":"model"}`, "since"},
		{"bad until", `{"by":"model","since":"7d","until":"3d"}`, `until "3d"`},
		{"until before since", `{"by":"model","since":"2026-02-01","until":"2026-01-01"}`, "before since"},
		{"limit too large", `{"by":"model","since":"7d","limit":501}`, "limit"},
		{"over a year", `{"by":"model","since":"367d"}`, "366 days"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := s.Dispatch(t.Context(), alice, proto.OpCost, json.RawMessage(tc.args))
			if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, "sfx cost -h") {
				t.Fatalf("cost(%s) = %+v; want invalid naming %q, fix with sfx cost -h", tc.args, perr, tc.msg)
			}
		})
	}
}

// The longest window back from now passes on a clock that moves between
// readings, as a real one does: both ends come from one reading. Taking
// since in the server and until in the store refused since 366d always.
func TestDispatchCostLongestWindow(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s, _ := newServerClock(t, Limits{}, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Millisecond)
		return clock
	})
	r := mustCall[proto.CostResult](t, s, bob, proto.OpCost, proto.CostArgs{By: "model", Since: "366d"})
	if got, want := r.Until.Sub(r.Since), 366*24*time.Hour; got != want {
		t.Errorf("cost since 366d covers %s, want %s", got, want)
	}
}

// A report names at most MaxUsageModels unpriced models and counts the
// rest, so a client can say how many it does not list.
func TestDispatchCostUnpricedMore(t *testing.T) {
	s := newServer(t)
	now := time.Now().UTC()
	var recs []proto.UsageRecord
	for i := range proto.MaxUsageModels + 2 {
		r := usageRec(fmt.Sprintf("r%d", i), now, 10)
		r.Model = fmt.Sprintf("mystery-%02d", i)
		recs = append(recs, r)
	}
	mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, proto.UsageArgs{Records: recs})
	r := mustCall[proto.CostResult](t, s, bob, proto.OpCost, proto.CostArgs{By: "model", Since: "1h"})
	if len(r.Unpriced) != proto.MaxUsageModels || r.UnpricedMore != 2 {
		t.Errorf("cost names %d unpriced models and %d more, want %d and 2", len(r.Unpriced), r.UnpricedMore, proto.MaxUsageModels)
	}
}

// A server with no prices sends no cost in show and digest, rather than
// calling every token unpriced.
func TestDispatchUsageCostWithoutPrices(t *testing.T) {
	s := newServer(t)
	is := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: is.ID})
	mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, proto.UsageArgs{Records: []proto.UsageRecord{usageRec("r1", time.Now().UTC(), 10)}})
	if u := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID}).Usage; u == nil || u.CostUSD != "" || u.Unpriced {
		t.Errorf("show(%s).usage = %+v, want tokens and no cost", is.ID, u)
	}
	if d := mustCall[proto.DigestResult](t, s, alice, proto.OpDigest, proto.DigestArgs{Since: "1h"}).Usage; d == nil || d.CostUSD != "" || d.Unpriced {
		t.Errorf("digest.usage = %+v, want tokens and no cost", d)
	}
}
