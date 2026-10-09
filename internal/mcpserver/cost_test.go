package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func costReply(groups []proto.CostGroup) func(string, any) (any, error) {
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return func(op string, args any) (any, error) {
		if op != proto.OpCost {
			return nil, errors.New("unexpected " + op)
		}
		a := args.(proto.CostArgs)
		gs := groups
		if a.Limit < len(gs) {
			gs = append(gs[:a.Limit:a.Limit], proto.CostGroup{Key: proto.OtherModels, Tokens: proto.Tokens{Input: n64(1)}, CostUSD: "0.5"})
		}
		return proto.CostResult{By: a.By, Since: since, Until: since.Add(7 * 24 * time.Hour), Groups: gs,
			Total:    proto.CostGroup{Tokens: proto.Tokens{Input: n64(4_300_042)}, CostUSD: "8.35", Unpriced: true},
			Unpriced: []string{"example-unpriced"}}, nil
	}
}

func TestCostTool(t *testing.T) {
	f := &fakeConn{reply: costReply([]proto.CostGroup{
		{Key: "sf-1", Title: "Client work", Tokens: proto.Tokens{Input: n64(4_300_000)}, CostUSD: "8.35", Split: true},
		{Key: proto.CostUnattributed, Tokens: proto.Tokens{Input: n64(42)}, CostUSD: "0", Unpriced: true},
	})}
	cs, _ := connect(t, f)
	raw := text(t, callTool(t, cs, "cost", map[string]any{"since": "7d", "by": "epic"}))
	var c Cost
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	want := Cost{By: "epic", Since: "2026-10-01T00:00Z", Until: "2026-10-08T00:00Z",
		Groups:   []string{"sf-1 Client work: $8.35, 4.3M tokens, split", "(unattributed): unpriced, 42 tokens"},
		Total:    "$8.35, some unpriced, 4.3M tokens",
		Unpriced: []string{"example-unpriced"}}
	want.set(true)
	if fmt.Sprint(c) != fmt.Sprint(want) {
		t.Errorf("cost =\n%+v\nwant\n%+v", c, want)
	}
	if args := f.calls[0].args.(proto.CostArgs); args != (proto.CostArgs{By: "epic", Since: "7d", Limit: costGroups}) {
		t.Errorf("cost sent %+v, want by epic since 7d limit %d", args, costGroups)
	}

	// by defaults to account.
	callTool(t, cs, "cost", map[string]any{"since": "2026-10-01", "until": "2026-10-08"})
	if args := f.calls[1].args.(proto.CostArgs); args.By != "account" || args.Until != "2026-10-08" {
		t.Errorf("cost without by sent %+v, want by account until 2026-10-08", args)
	}
}

// A report too large for MaxResultTokens keeps its first groups and sums
// the rest, the server's (other) included, into one (other), so the
// groups still add up to the total. It takes one call: asking again with
// fewer groups ran up to five full reports.
func TestCostToolFits(t *testing.T) {
	var groups []proto.CostGroup
	for i := range 60 { // principals as long as their pattern allows
		groups = append(groups, proto.CostGroup{Key: fmt.Sprintf("p%03d%s", i, strings.Repeat("x", 251)),
			Tokens: proto.Tokens{Input: n64(int64(1000 - i)), Output: n64(1)}, CostUSD: "0.335", Unpriced: true})
	}
	groups[costGroups-1].Split = true // in the tail, so (other) is split
	f := &fakeConn{reply: costReply(groups)}
	cs, _ := connect(t, f)
	raw := text(t, callTool(t, cs, "cost", map[string]any{"since": "7d", "by": "person"}))
	if got := Tokens([]byte(raw)); got > MaxResultTokens {
		t.Errorf("cost result is ~%d tokens, over %d", got, MaxResultTokens)
	}
	if len(f.calls) != 1 {
		t.Errorf("cost made %d calls, want 1", len(f.calls))
	}
	var c Cost
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	kept := len(c.Groups) - 1
	if kept < 1 || kept >= costGroups {
		t.Fatalf("cost kept %d of %d groups, want some folded into (other)", kept, costGroups)
	}
	// The server sent costGroups groups at 0.335 USD and 1001-i tokens
	// each, then (other) at 0.5 USD and 1 token.
	folded := int64(costGroups - kept)
	tokens := int64(1)
	for i := kept; i < costGroups; i++ {
		tokens += 1001 - int64(i)
	}
	usd := proto.USD(big.NewInt(folded*335_000_000_000 + 500_000_000_000))
	want := fmt.Sprintf("%s: %s, some unpriced, %s tokens, split", proto.OtherModels, proto.Dollars(usd), proto.TokenCount(tokens))
	if got := c.Groups[kept]; got != want || c.Total != "$8.35, some unpriced, 4.3M tokens" {
		t.Errorf("cost kept %d groups, then %q, total %q; want %q, the total whole", kept, got, c.Total, want)
	}
}

// The tool names five unpriced models and counts the rest, the ones the
// server did not name included.
func TestCostToolUnpricedMore(t *testing.T) {
	f := &fakeConn{reply: func(string, any) (any, error) {
		return proto.CostResult{By: "model", Groups: []proto.CostGroup{}, Total: proto.CostGroup{CostUSD: "0"},
			Unpriced: []string{"m1", "m2", "m3", "m4", "m5", "m6"}, UnpricedMore: 2}, nil
	}}
	cs, _ := connect(t, f)
	var c Cost
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "cost", map[string]any{"since": "7d"}))), &c); err != nil {
		t.Fatal(err)
	}
	if want := []string{"m1", "m2", "m3", "m4", "m5", "3 more models"}; !slices.Equal(c.Unpriced, want) {
		t.Errorf("cost unpriced = %q, want %q", c.Unpriced, want)
	}
}

// With plans and hours, a group's line carries its amortized cost and
// its logged hours, and folding sums both exactly.
func TestCostToolAmortizedAndLogged(t *testing.T) {
	f := &fakeConn{reply: func(string, any) (any, error) {
		return proto.CostResult{By: "issue", Groups: []proto.CostGroup{
			{Key: "sf-1", Title: "work", Tokens: proto.Tokens{Input: n64(1000)}, CostUSD: "8.35", AmortizedUSD: "20.5", LoggedSeconds: 5400},
			{Key: proto.CostUnattributed, Tokens: proto.Tokens{Input: n64(10)}, CostUSD: "0.1", AmortizedUSD: "9.5"},
		}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n64(1010)}, CostUSD: "8.45", AmortizedUSD: "30", LoggedSeconds: 5400}}, nil
	}}
	cs, _ := connect(t, f)
	var c Cost
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "cost", map[string]any{"since": "7d", "by": "issue"}))), &c); err != nil {
		t.Fatal(err)
	}
	want := []string{"sf-1 work: $8.35, amortized $20.50, 1k tokens, 1.5h logged", "(unattributed): $0.10, amortized $9.50, 10 tokens"}
	if !slices.Equal(c.Groups, want) || c.Total != "$8.45, amortized $30.00, 1k tokens, 1.5h logged" {
		t.Errorf("cost groups %q, total %q; want %q and the total", c.Groups, c.Total, want)
	}
	g, err := fold([]proto.CostGroup{
		{Key: "a", CostUSD: "1", AmortizedUSD: "0.000001", LoggedSeconds: 60},
		{Key: "b", CostUSD: "2", AmortizedUSD: "3", LoggedSeconds: 120},
	})
	if err != nil || g.AmortizedUSD != "3.000001" || g.LoggedSeconds != 180 {
		t.Errorf("fold = %+v, %v; want 3.000001 USD amortized and 180 s logged", g, err)
	}
	if g, err := fold([]proto.CostGroup{{Key: "a", CostUSD: "1"}}); err != nil || g.AmortizedUSD != "" {
		t.Errorf("fold with no plans = %+v, %v; want no amortized cost", g, err)
	}
}
