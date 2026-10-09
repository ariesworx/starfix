package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
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

// A report too large for MaxResultTokens is asked for again with fewer
// groups, so the server sums the rest into (other) and the total stays
// whole.
func TestCostToolFits(t *testing.T) {
	var groups []proto.CostGroup
	for i := range 60 { // principals as long as their pattern allows
		groups = append(groups, proto.CostGroup{Key: fmt.Sprintf("p%03d%s", i, strings.Repeat("x", 251)),
			Tokens: proto.Tokens{Input: n64(int64(1000 - i))}, CostUSD: "1", Unpriced: true, Split: true})
	}
	f := &fakeConn{reply: costReply(groups)}
	cs, _ := connect(t, f)
	raw := text(t, callTool(t, cs, "cost", map[string]any{"since": "7d", "by": "person"}))
	if got := Tokens([]byte(raw)); got > MaxResultTokens {
		t.Errorf("cost result is ~%d tokens, over %d", got, MaxResultTokens)
	}
	var c Cost
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	last := c.Groups[len(c.Groups)-1]
	if len(f.calls) < 2 || !strings.HasPrefix(last, proto.OtherModels+": $0.50") || c.Total != "$8.35, some unpriced, 4.3M tokens" {
		t.Errorf("after %d calls: %d groups, last %q, total %q; want fewer groups asked again, (other) last, the total whole",
			len(f.calls), len(c.Groups), last, c.Total)
	}
}
