package mcpserver

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
)

// costBys are the groupings the cost tool takes.
var costBys = []any{"account", "issue", "epic", "person", "model"}

// costGroups is how many groups the cost tool asks for; the server sums
// the rest into (other).
const costGroups = 20

// CostIn selects a cost report.
type CostIn struct {
	By    string `json:"by,omitempty"`
	Since string `json:"since" jsonschema:"7d or a date"`
	Until string `json:"until,omitempty"`
}

// Cost is a cost report in lines: each group's list-price cost and
// tokens, the total, and the models with no price.
type Cost struct {
	untrusted
	By        string   `json:"by"`
	Since     string   `json:"since"`
	Until     string   `json:"until"`
	Groups    []string `json:"groups"`
	Total     string   `json:"total"`
	Unpriced  []string `json:"unpriced,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (r *Cost) mark() { r.set(len(r.Groups) > 0) }

func (s *Server) registerCost() {
	add(s, tool{name: "cost", desc: "Token cost at list price.", ann: readOnly, retry: true,
		enums: enums{"by": costBys}},
		func(ctx context.Context, c Conn, in CostIn) (Cost, error) { return cost(ctx, c, in) })
}

// cost is the cost tool. A report that would pass MaxResultTokens keeps
// as many groups as fit and sums the rest, the server's (other)
// included, into one (other), so the groups still add up to the total.
func cost(ctx context.Context, c Conn, in CostIn) (Cost, error) {
	var r proto.CostResult
	args := proto.CostArgs{By: orDefault(in.By, "account"), Since: in.Since, Until: in.Until, Limit: costGroups}
	if err := c.Call(ctx, proto.OpCost, args, &r); err != nil {
		return Cost{}, err
	}
	out := Cost{By: r.By, Since: stamp(r.Since), Until: stamp(r.Until), Groups: []string{}, Truncated: r.Truncated,
		Total: costText(r.Total.CostUSD, r.Total.Unpriced) + ", " + proto.TokenCount(tokenSum(r.Total.Tokens)) + " tokens"}
	for _, m := range r.Unpriced[:min(len(r.Unpriced), usageModels)] {
		name, _ := cut(m, usageModelLen)
		out.Unpriced = append(out.Unpriced, name)
	}
	if more := len(r.Unpriced) - usageModels; more > 0 {
		out.Unpriced = append(out.Unpriced, fmt.Sprintf("%d more models", more))
	}
	for _, g := range r.Groups {
		out.Groups = append(out.Groups, groupLine(g))
	}
	// Fold one more group into (other) until the result fits. Each try
	// is local, and there are at most costGroups+1 groups.
	for keep := len(r.Groups) - 1; size(out) > MaxResultTokens && keep >= 0; keep-- {
		other, err := fold(r.Groups[keep:])
		if err != nil {
			return Cost{}, err
		}
		out.Groups = append(out.Groups[:keep], groupLine(other))
	}
	return out, nil
}

// fold sums groups into one (other) group: their exact cost, the counts
// any of them knows, and whether any was unpriced or split.
func fold(gs []proto.CostGroup) (proto.CostGroup, error) {
	out := proto.CostGroup{Key: proto.OtherModels}
	pico := new(big.Int)
	for _, g := range gs {
		n, err := proto.ParseUSD(g.CostUSD)
		if err != nil {
			return out, fmt.Errorf("cost of %s: %w", g.Key, err)
		}
		pico.Add(pico, n)
		for _, c := range []struct{ in, out **int64 }{
			{&g.Input, &out.Input}, {&g.Output, &out.Output}, {&g.CacheWrite, &out.CacheWrite},
			{&g.CacheWrite1h, &out.CacheWrite1h}, {&g.CacheRead, &out.CacheRead},
		} {
			if *c.in == nil {
				continue
			}
			if *c.out == nil {
				*c.out = new(int64)
			}
			**c.out += **c.in
		}
		out.Unpriced = out.Unpriced || g.Unpriced
		out.Split = out.Split || g.Split
	}
	out.CostUSD = proto.USD(pico)
	return out, nil
}

// groupLine is one group: "KEY TITLE: $8.35, 4.3M tokens, split".
func groupLine(g proto.CostGroup) string {
	head := g.Key
	if g.Title != "" {
		title, _ := cut(strings.Join(strings.Fields(g.Title), " "), usageModelLen)
		head += " " + title
	}
	line := head + ": " + costText(g.CostUSD, g.Unpriced) + ", " + proto.TokenCount(tokenSum(g.Tokens)) + " tokens"
	if g.Split {
		line += ", split"
	}
	return line
}

// costText is a cost for an agent: "$8.35", "$8.35, some unpriced", or
// "unpriced" when no token had a price, which is not free.
func costText(usd string, unpriced bool) string {
	switch {
	case unpriced && strings.Trim(usd, "0.") == "":
		return "unpriced"
	case unpriced:
		return proto.Dollars(usd) + ", some unpriced"
	}
	return proto.Dollars(usd)
}

// tokenSum is the counts t knows, added up.
func tokenSum(t proto.Tokens) int64 { return total(proto.ModelTokens{Tokens: t}) }
